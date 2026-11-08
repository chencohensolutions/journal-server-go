package importcmd

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEmptyFoldersAreRemoved: after a run, folders left empty in the searched folders go, deepest first; folders with
// anything in them, hidden ones, the imported folder and the searched folder itself stay; a dry run removes nothing.
func TestEmptyFoldersAreRemoved(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "source")
	mkdir := func(path string) string {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	emptyNested := mkdir(filepath.Join(root, "2021", "DCIM", "Audio"))
	keptFile := write(t, filepath.Join(root, "2022", "notes.txt"), []byte("not a photo"))
	keptHiddenFile := write(t, filepath.Join(root, "2023", ".nomedia"), []byte{})
	hidden := mkdir(filepath.Join(root, ".thumbnails"))
	imported := mkdir(filepath.Join(root, "Imported", "run"))

	im := &importer{roots: []string{root}, importedFolder: filepath.Join(root, "Imported"), dryRun: true}
	im.removeEmptyFolders()
	if _, err := os.Stat(emptyNested); err != nil {
		t.Fatalf("a dry run removed a folder: %v", err)
	}

	im.dryRun = false
	im.removeEmptyFolders()
	for _, gone := range []string{emptyNested, filepath.Join(root, "2021")} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s is still there", gone)
		}
	}
	for _, kept := range []string{root, filepath.Dir(keptFile), filepath.Dir(keptHiddenFile), hidden, imported} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s was removed", kept)
		}
	}
	if im.counts.emptyFolders != 3 {
		t.Errorf("counted %d, want 3 (Audio, DCIM, 2021)", im.counts.emptyFolders)
	}
}
