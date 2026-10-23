//go:build windows

package copies

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// sameDisk reports whether a and b are on one volume (C:\, or the mount folder of a volume).
func sameDisk(a, b string) (bool, error) {
	va, err := volume(a)
	if err != nil {
		return false, err
	}
	vb, err := volume(b)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(va, vb), nil
}

func volume(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	name, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(name, &buf[0], uint32(len(buf))); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf), nil
}
