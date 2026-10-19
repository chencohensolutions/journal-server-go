//go:build unix

package media

import "golang.org/x/sys/unix"

// freeBytes is the space available to this process on the disk holding path.
func freeBytes(path string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}
