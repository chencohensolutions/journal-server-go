package media

import "path/filepath"

// JXLLibraries is where libjxl may be: configured, else beside the vips program found (vips/bin from
// scripts/bundle-vips has it, for libvips's own JPEG XL support).
func JXLLibraries(vips, configured string) []string {
	if configured != "" {
		return []string{configured}
	}
	if vips == "" {
		return []string{filepath.Join("vips", "bin", "libjxl.dll")}
	}
	return []string{filepath.Join(filepath.Dir(vips), "libjxl.dll")}
}
