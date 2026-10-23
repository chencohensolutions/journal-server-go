//go:build !darwin && !linux && !freebsd && !netbsd && !windows

package media

import "errors"

func vipsLibraries(vips, configured string) []string { return []string{configured} }

func openVipsLibraries([]string) ([]uintptr, error) {
	return nil, errors.New("loading libvips is not supported on this system")
}

func findVipsSymbol(uintptr, string) (uintptr, error) { return 0, errors.ErrUnsupported }
