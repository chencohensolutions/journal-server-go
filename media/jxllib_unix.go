//go:build darwin || linux || freebsd || netbsd

package media

import (
	"path/filepath"
	"runtime"
)

// JXLLibraries is where libjxl may be: configured, else beside the vips program found (Homebrew's vips depends on
// jpeg-xl, in ../lib), else the system's by name.
func JXLLibraries(vips, configured string) []string {
	if configured != "" {
		return []string{configured}
	}
	names := []string{"libjxl.so", "libjxl.so.0.11", "libjxl.so.0.10", "libjxl.so.0.9", "libjxl.so.0.8"}
	if runtime.GOOS == "darwin" {
		names = []string{"libjxl.dylib"}
	}
	var libs []string
	if vips != "" {
		for _, name := range names {
			libs = append(libs, filepath.Join(filepath.Dir(vips), "..", "lib", name))
		}
	}
	return append(libs, names...)
}
