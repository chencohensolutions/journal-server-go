package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/version"
)

const configPath = "config.json"

const usage = `usage:
  journal-server                      run the server
  journal-server version              the version (version/VERSION) and build
  journal-server enroll-code <user>   create a one-time enrollment code
  journal-server devices [user]       list enrolled devices
  journal-server revoke <serial>      revoke an enrolled device
  journal-server private <serial> withhold|allow
                                      keep private files from a device, or send them to it again
  journal-server index                read new files for the indexes once, then exit (for "index": "daily";
                                      the server starts it at "index_at")
  journal-server index --only nudity,faces
                                      the same, with only the indexes named (nudity, faces, ocr, search,
                                      similar, aesthetic), to bring those up to date first
  journal-server index --user <name>  the same, for one user's files only
  journal-server index --progress     also write how the run is going to stdout, for the server that started it
  journal-server copy [user]          start a full pass of the user's copies now (config.json "copies")
  journal-server copy report [user] [--list]
                                      how each copy is doing, and what it lacks now
  journal-server setup ...            make a server folder: config, certificates, downloads (docs/install.md;
                                      journal-server setup --help)
  journal-server service install|uninstall|start|stop
                                      the Windows service that starts the server at boot (as administrator)
`

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println("journal-server", version.String())
		return
	}
	// These two run before there is a config.json (setup makes it).
	if len(os.Args) >= 2 && (os.Args[1] == "setup" || os.Args[1] == "service") {
		run := runSetup
		if os.Args[1] == "service" {
			run = runServiceCommand
		}
		if err := run(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if runAsService() {
		return
	}
	if _, err := auth.UseInstallFolder(configPath); err != nil {
		log.Fatal(err)
	}
	config, err := auth.LoadConfig(configPath)
	if err != nil {
		log.Fatal(err)
	}

	if len(os.Args) < 2 {
		serve(context.Background(), config, exitStopped)
		return
	}
	if os.Args[1] == "index" {
		args, err := parseIndexArgs(os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(2)
		}
		if err := runIndex(config, args); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := runAdminCommand(config, os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
