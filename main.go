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
	if err := runAdminCommand(config, os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
