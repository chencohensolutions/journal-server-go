//go:build windows

package media

import "golang.org/x/sys/windows"

// freeBytes is the space available to this process on the disk holding path.
func freeBytes(path string) (uint64, error) {
	dir, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var available, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(dir, &available, &total, &free); err != nil {
		return 0, err
	}
	return available, nil
}
