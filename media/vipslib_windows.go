package media

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// vipsLibraries is the libvips library to load, then the GLib libraries its functions for values and objects are
// in: those beside the vips program found (vips/bin from scripts/bundle-vips), or the configured library's folder.
func vipsLibraries(vips, configured string) []string {
	dir := filepath.Dir(vips)
	lib := filepath.Join(dir, "libvips-42.dll")
	if configured != "" {
		lib, dir = configured, filepath.Dir(configured)
	}
	libs := []string{lib}
	for _, name := range []string{"libgobject-2.0-0.dll", "libglib-2.0-0.dll"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			libs = append(libs, filepath.Join(dir, name))
		}
	}
	return libs
}

func openVipsLibraries(libs []string) ([]uintptr, error) {
	// libvips loads its HEIF and JPEG XL support later, as modules (vips/lib/vips-modules-*), and Windows finds
	// the DLLs those need in the server's folder, then the system's, then the PATH. Another libvips on the PATH (the
	// official Windows build, whose libheif has no HEVC) would then be mixed in: its folder is searched first.
	for i, lib := range libs {
		abs, err := filepath.Abs(lib) // both need whole paths
		if err != nil {
			return nil, err
		}
		libs[i] = abs
	}
	if err := windows.SetDllDirectory(filepath.Dir(libs[0])); err != nil {
		return nil, err
	}
	var handles []uintptr
	for _, lib := range libs {
		// The altered search path finds the DLLs each one needs beside it.
		h, err := windows.LoadLibraryEx(lib, 0, windows.LOAD_WITH_ALTERED_SEARCH_PATH)
		if err != nil {
			return nil, err
		}
		handles = append(handles, uintptr(h))
	}
	return handles, nil
}

func findVipsSymbol(lib uintptr, name string) (uintptr, error) {
	return windows.GetProcAddress(windows.Handle(lib), name)
}
