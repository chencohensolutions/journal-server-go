//go:build !darwin && !linux && !freebsd && !netbsd && !windows

package media

func JXLLibraries(vips, configured string) []string { return []string{configured} }
