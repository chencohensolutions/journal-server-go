package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUseInstallFolderKeepsAFolderWithItsOwnConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("config.json", []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := UseInstallFolder("config.json")
	if err != nil || changed != "" {
		t.Fatalf("changed to %q (%v) though config.json is here", changed, err)
	}
	// The test binary's folder has no config.json either, so a folder without one stays as it is too.
	empty := t.TempDir()
	t.Chdir(empty)
	if changed, err := UseInstallFolder("config.json"); err != nil || changed != "" {
		t.Fatalf("changed to %q (%v) with no config.json beside the program", changed, err)
	}
	if changed, err := UseInstallFolder(filepath.Join(dir, "config.json")); err != nil || changed != "" {
		t.Fatalf("changed to %q (%v) for an absolute path", changed, err)
	}
}
