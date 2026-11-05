package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/journal/server-go/auth"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// serviceName is the Windows service the installer sets up (docs/install.md): it starts the server at boot, before
// anyone logs in, and again if it stops on an error.
const serviceName = "JournalServer"

const serviceUsage = `usage: journal-server service install|uninstall|start|stop   (as administrator)`

// runAsService runs the server as the Windows service when Windows started it as one, and reports whether it did.
// Services start in System32, so it first changes to the program's folder, where config.json and everything it
// names are, and writes the log to server.log there.
func runAsService() bool {
	isService, err := svc.IsWindowsService()
	if err != nil || !isService {
		return false
	}
	exe, err := os.Executable()
	if err == nil {
		err = os.Chdir(filepath.Dir(exe))
	}
	if logFile, openErr := os.OpenFile("server.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); openErr == nil {
		log.SetOutput(logFile)
		os.Stdout, os.Stderr = logFile, logFile
	}
	if err != nil {
		log.Fatalf("service: %v", err)
	}
	if err := svc.Run(serviceName, serverService{}); err != nil {
		log.Fatalf("service: %v", err)
	}
	return true
}

// serverService is the server as a Windows service: Stop and Shutdown end it the way Ctrl+C does.
type serverService struct{}

func (serverService) Execute(args []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	config, err := auth.LoadConfig(configPath)
	if err != nil {
		log.Printf("service: %v", err)
		return true, 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go serve(ctx, config, func() { close(stopped) })
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case <-stopped:
			return false, 0
		case r := <-requests:
			switch r.Cmd {
			case svc.Interrogate:
				status <- r.CurrentStatus
			case svc.Stop, svc.Shutdown:
				// The reader may take up to 30 seconds to finish its file (stopOnSignal).
				status <- svc.Status{State: svc.StopPending, WaitHint: 40000}
				cancel()
				<-stopped
				return false, 0
			}
		}
	}
}

// runServiceCommand installs, removes, starts or stops the Windows service.
func runServiceCommand(args []string) error {
	if len(args) != 1 {
		return errors.New(serviceUsage)
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("can't reach the service manager (run as administrator): %w", err)
	}
	defer m.Disconnect()
	switch args[0] {
	case "install":
		if err := installService(m); err != nil {
			return err
		}
		return allowThroughFirewall()
	case "uninstall":
		removeFirewallRule()
		s, err := m.OpenService(serviceName)
		if err != nil {
			return nil // not installed
		}
		defer s.Close()
		stopService(s)
		return s.Delete()
	case "start":
		s, err := m.OpenService(serviceName)
		if err != nil {
			return err
		}
		defer s.Close()
		return s.Start()
	case "stop":
		s, err := m.OpenService(serviceName)
		if err != nil {
			return err
		}
		defer s.Close()
		stopService(s)
		return nil
	}
	return errors.New(serviceUsage)
}

// installService registers the service (or updates it to this program), started automatically shortly after boot
// and restarted when it stops on an error.
func installService(m *mgr.Mgr) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	config := mgr.Config{
		DisplayName:      "Journal server",
		Description:      "The private photo journal's server (journal-server.exe in " + filepath.Dir(exe) + ").",
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true, // after the network is up
	}
	s, err := m.OpenService(serviceName)
	if err == nil {
		defer s.Close()
		current, err := s.Config()
		if err != nil {
			return err
		}
		current.BinaryPathName = windows.EscapeArg(exe)
		current.DisplayName, current.Description = config.DisplayName, config.Description
		current.StartType, current.DelayedAutoStart = config.StartType, config.DelayedAutoStart
		if err := s.UpdateConfig(current); err != nil {
			return err
		}
	} else {
		s, err = m.CreateService(serviceName, exe, config)
		if err != nil {
			return err
		}
		defer s.Close()
	}
	// Restart after 10 seconds, then a minute; the count of failures resets after a day.
	return s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: time.Minute},
		{Type: mgr.ServiceRestart, Delay: time.Minute},
	}, uint32((24 * time.Hour).Seconds()))
}

// stopService asks the service to stop and waits up to a minute for it to.
func stopService(s *mgr.Service) {
	status, err := s.Control(svc.Stop)
	if err != nil {
		return // not running
	}
	for deadline := time.Now().Add(time.Minute); status.State != svc.Stopped && time.Now().Before(deadline); {
		time.Sleep(500 * time.Millisecond)
		if status, err = s.Query(); err != nil {
			return
		}
	}
}

// firewallRule names the Windows Firewall rule that lets devices reach the server's port.
const firewallRule = "Journal server"

// allowThroughFirewall lets devices on the network reach the port in config.json (beside this program), replacing
// the rule an earlier install made.
func allowThroughFirewall() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	config, err := auth.LoadConfig(filepath.Join(filepath.Dir(exe), configPath))
	if err != nil {
		return err
	}
	removeFirewallRule()
	out, err := exec.Command("netsh", "advfirewall", "firewall", "add", "rule", "name="+firewallRule, "dir=in",
		"action=allow", "protocol=TCP", "localport="+config.Port, "program="+exe).CombinedOutput()
	if err != nil {
		return fmt.Errorf("firewall rule: %v: %s", err, out)
	}
	return nil
}

func removeFirewallRule() {
	exec.Command("netsh", "advfirewall", "firewall", "delete", "rule", "name="+firewallRule).Run()
}
