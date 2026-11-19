//go:build !windows

package main

// relaunchElevated is for Windows, where the service needs an administrator; elsewhere setup goes on as it is.
func relaunchElevated([]string) (bool, error) { return false, nil }
