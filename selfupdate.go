package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/update"
	"github.com/journal/server-go/version"
)

// startedAs is this program's path as it started. After an update renames the running program to .old, the
// operating system may report that name instead, so the path is taken before anything is renamed.
var startedAs, startedAsErr = executablePath()

func executablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// restarting is set when the server stops to start again as the newer program an update put in its place.
var restarting atomic.Bool

// updateDaily looks for a newer release once soon after start, to write to the log whether one is out, and every
// day at config.UpdateAt, when with "update": "auto" it installs one of the same major version and calls restart.
func updateDaily(ctx context.Context, config auth.AppConfig, restart func()) {
	if config.Update == auth.UpdateOff {
		return
	}
	if !version.Released() {
		log.Printf("Updates: this is a build from source, which doesn't update itself")
		return
	}
	u := update.New(version.Number)
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Minute):
	}
	checkForUpdate(ctx, config, u, false, restart)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(nextRun(config.UpdateAt, time.Now()))):
		}
		checkForUpdate(ctx, config, u, config.Update == auth.UpdateAuto, restart)
	}
}

// checkForUpdate logs whether a newer release is out and, when install is set, installs it and calls restart.
func checkForUpdate(ctx context.Context, config auth.AppConfig, u *update.Updater, install bool, restart func()) {
	r, err := u.Latest(ctx)
	if err != nil {
		log.Printf("Updates: %v", err)
		return
	}
	newer, major := u.Newer(r)
	switch {
	case !newer:
		return
	case major:
		log.Printf("Updates: %s is out, a new major version (this is %s); install it with journal-server update after reading %s",
			r.Version, version.Number, r.URL)
		return
	case !install:
		when := "with journal-server update"
		if config.Update == auth.UpdateAuto {
			when = "at " + config.UpdateAt
		}
		log.Printf("Updates: %s is out (this is %s); it is installed %s", r.Version, version.Number, when)
		return
	}
	log.Printf("Updates: installing %s (this is %s)", r.Version, version.Number)
	if err := installRelease(ctx, u, r); err != nil {
		log.Printf("Updates: %s not installed: %v", r.Version, err)
		return
	}
	log.Printf("Updates: %s installed; restarting", r.Version)
	restart()
}

// installRelease downloads and checks r's program, puts it in place of this one (kept as .old), and runs its
// setup in the install folder, which downloads anything the new version needs that isn't there yet.
func installRelease(ctx context.Context, u *update.Updater, r *update.Release) error {
	if startedAsErr != nil {
		return startedAsErr
	}
	downloaded, err := u.Download(ctx, r, startedAs)
	if err != nil {
		return err
	}
	if err := update.Install(startedAs, downloaded); err != nil {
		os.Remove(downloaded)
		return err
	}
	dir := filepath.Dir(startedAs)
	if _, err := os.Stat(filepath.Join(dir, configPath)); err == nil {
		cmd := exec.CommandContext(ctx, startedAs, "setup", "--dir", dir)
		cmd.Stdout, cmd.Stderr = log.Writer(), log.Writer()
		if err := cmd.Run(); err != nil {
			// The new version still runs; whatever it lacks answers 503 until setup is run again.
			log.Printf("Updates: setup of %s didn't finish (run journal-server setup --dir %s): %v", r.Version, dir, err)
		}
	}
	return nil
}

// startAgain runs the new program in place of this process, once the server has stopped for an update: on Unix
// in the same process (so launchd or systemd see no change), on Windows as a new process with the same console.
// The Windows service doesn't come here: it exits and the service manager starts it again (service_windows.go).
func startAgain() {
	args := append([]string{startedAs}, os.Args[1:]...)
	if runtime.GOOS != "windows" {
		err := syscall.Exec(startedAs, args, os.Environ())
		log.Fatalf("Updates: can't start the new program: %v", err)
	}
	cmd := exec.Command(startedAs, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		log.Fatalf("Updates: can't start the new program: %v", err)
	}
	os.Exit(0)
}

const updateUsage = `usage: journal-server update            install the latest release now and restart the server
       journal-server update --check    only say whether a newer release is out
       journal-server update --rollback go back to the program the last update replaced (again: undo that)`

// updateCommand is journal-server update: by hand, any newer release (a new major version too), then the
// Windows service is restarted when it runs (as administrator); otherwise the server is to be restarted by hand.
func updateCommand(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(fs.Output(), updateUsage) }
	check := fs.Bool("check", false, "")
	rollback := fs.Bool("rollback", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 || *check && *rollback {
		return errors.New(updateUsage)
	}
	if !version.Released() {
		return errors.New("this is a build from source, which doesn't update itself: git pull and build it again")
	}
	if startedAsErr != nil {
		return startedAsErr
	}
	if *rollback {
		if err := update.Rollback(startedAs); err != nil {
			return err
		}
		fmt.Println("The earlier program is back in place.")
		return restartAfterCommand()
	}
	ctx := context.Background()
	u := update.New(version.Number)
	r, err := u.Latest(ctx)
	if err != nil {
		return err
	}
	if newer, _ := u.Newer(r); !newer {
		fmt.Printf("This is the latest release (%s).\n", version.Number)
		return nil
	}
	if *check {
		fmt.Printf("%s is out (this is %s): %s\n", r.Version, version.Number, r.URL)
		return nil
	}
	fmt.Printf("Installing %s (this is %s)...\n", r.Version, version.Number)
	log.SetOutput(os.Stdout)
	if err := installRelease(ctx, u, r); err != nil {
		return err
	}
	fmt.Printf("%s installed; %s.old is the program it replaced (journal-server update --rollback).\n",
		r.Version, filepath.Base(startedAs))
	return restartAfterCommand()
}

func restartAfterCommand() error {
	restarted, err := restartService()
	if err != nil {
		return err
	}
	if restarted {
		fmt.Println("The JournalServer service started again with it.")
	} else {
		fmt.Println("Restart the server to run it.")
	}
	return nil
}
