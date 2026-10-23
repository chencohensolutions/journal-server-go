package main

import (
	"fmt"
	"log"
	"os"

	"github.com/journal/server-go/auth"
)

const configPath = "config.json"

const usage = `usage:
  journal-server                      run the server
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
  journal-server copy [user]          start a full pass of the user's copies now (config.json "copies")
  journal-server copy report [user] [--list]
                                      how each copy is doing, and what it lacks now
`

func main() {
	config, err := auth.LoadConfig(configPath)
	if err != nil {
		log.Fatal(err)
	}

	if len(os.Args) < 2 {
		serve(config)
		return
	}
	if os.Args[1] == "index" {
		only, err := parseOnly(os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(2)
		}
		if err := runIndex(config, only); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := runAdminCommand(config, os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
