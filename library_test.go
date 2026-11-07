package main

import (
	"path/filepath"
	"testing"
)

// The warning about a libvips that is not the server's own vips folder: only on Windows, only when config.json
// names none.
func TestForeignVips(t *testing.T) {
	own := filepath.Join("vips", "bin", "vips.exe")
	abs, err := filepath.Abs(own)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "vips-dev-8.18", "bin", "vips.exe")
	beside := filepath.Join("vips-other", "bin", "vips.exe")

	for _, c := range []struct {
		name, goos, found, configured string
		warns                         bool
	}{
		{"own folder", "windows", own, "", false},
		{"own folder, absolute", "windows", abs, "", false},
		{"on the PATH", "windows", other, "", true},
		{"a folder whose name starts like vips", "windows", beside, "", true},
		{"named in config.json", "windows", other, other, false},
		{"not Windows", "darwin", "/opt/homebrew/bin/vips", "", false},
	} {
		if got := foreignVips(c.goos, c.found, c.configured); (got != "") != c.warns {
			t.Errorf("%s: warning %q, want one: %v", c.name, got, c.warns)
		}
	}
}
