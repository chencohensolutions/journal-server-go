package main

import (
	"encoding/json"
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
	config, err := loadConfig(configPath)
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

func loadConfig(path string) (auth.AppConfig, error) {
	configFile, err := os.Open(path)
	if err != nil {
		return auth.AppConfig{}, fmt.Errorf("failed to open %s: %w", path, err)
	}
	defer configFile.Close()

	var config auth.AppConfig
	if err := json.NewDecoder(configFile).Decode(&config); err != nil {
		return auth.AppConfig{}, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	return config.WithDefaults(), nil
}
