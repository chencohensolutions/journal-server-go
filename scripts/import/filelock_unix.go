//go:build !windows

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockFile waits until this process holds f's exclusive lock, which the operating system keeps across
// processes (and drops if the process dies).
func lockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX) }

func unlockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }
