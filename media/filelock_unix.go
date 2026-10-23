//go:build unix

package media

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockFile waits until this process holds f's exclusive lock, which the operating system keeps across
// processes (and drops if the process dies).
func lockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX) }

func unlockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }

// tryLockFile takes f's exclusive lock if no one holds it, and reports whether it did.
func tryLockFile(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == unix.EWOULDBLOCK {
		return false, nil
	}
	return err == nil, err
}
