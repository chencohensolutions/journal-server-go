package main

import (
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// relaunchElevated starts this program again as administrator with the same arguments (Windows asks the user), and
// reports true when it did, so this one stops. An administrator's process reports false and goes on.
func relaunchElevated(args []string) (bool, error) {
	if windows.GetCurrentProcessToken().IsElevated() {
		return false, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	quoted := make([]string, 0, len(args)+1)
	for _, a := range append([]string{"setup"}, args...) {
		quoted = append(quoted, syscall.EscapeArg(a))
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	params, _ := windows.UTF16PtrFromString(strings.Join(quoted, " "))
	if err := windows.ShellExecute(0, verb, file, params, nil, windows.SW_SHOWNORMAL); err != nil {
		return false, err
	}
	return true, nil
}
