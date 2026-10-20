//go:build !unix && !windows

package media

import "os"

// Without file locks, processes don't take turns: run one at a time.
func lockFile(*os.File) error   { return nil }
func unlockFile(*os.File) error { return nil }
