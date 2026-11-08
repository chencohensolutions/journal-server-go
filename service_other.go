//go:build !windows

package main

import "errors"

// runAsService reports whether the server ran as a Windows service; there are none here.
func runAsService() bool { return false }

func runServiceCommand(args []string) error {
	return errors.New("journal-server service is for Windows; elsewhere start journal-server with launchd or systemd")
}

// restartService restarts the Windows service; there is none here, so the server is restarted by its own manager.
func restartService() (bool, error) { return false, nil }
