//go:build darwin || linux || freebsd || netbsd

package media

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/ebitengine/purego"
)

// vipsLibraries is the libvips library to load: configured, else the one beside the vips program found (Homebrew
// keeps it in ../lib), else the system's by name. Looking up a function in it also finds GLib's, which it loads.
func vipsLibraries(vips, configured string) []string {
	if configured != "" {
		return []string{configured}
	}
	name := "libvips.so.42"
	if runtime.GOOS == "darwin" {
		name = "libvips.42.dylib"
	}
	beside := filepath.Join(filepath.Dir(vips), "..", "lib", name)
	if _, err := os.Stat(beside); err == nil {
		return []string{beside}
	}
	return []string{name}
}

func openVipsLibraries(libs []string) ([]uintptr, error) {
	var handles []uintptr
	for _, lib := range libs {
		h, err := purego.Dlopen(lib, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			return nil, err
		}
		handles = append(handles, h)
	}
	return handles, nil
}

func findVipsSymbol(lib uintptr, name string) (uintptr, error) {
	return purego.Dlsym(lib, name)
}
