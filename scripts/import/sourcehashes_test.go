package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMovedFilesKeepTheirHash: a file the import moves is known where it went, after the table is opened again (the
// next run), so importing that folder doesn't read it again.
func TestMovedFilesKeepTheirHash(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	hashes, err := openSourceHashes(data)
	if err != nil {
		t.Fatal(err)
	}
	from := write(t, filepath.Join(dir, "source", "IMG_1.jpg"), []byte("a photo"))
	info, _ := os.Stat(from)
	sha, err := hashes.sha256(from, info)
	if err != nil {
		t.Fatal(err)
	}
	im := &importer{sources: hashes}
	to, err := im.move(from, filepath.Join(dir, "imported", "run", "IMG_1.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	hashes.close()

	again, err := openSourceHashes(data)
	if err != nil {
		t.Fatal(err)
	}
	defer again.close()
	known, ok := again.hashes[to]
	if !ok || known.sha256 != sha {
		t.Fatalf("the moved file isn't known where it went: %+v", known)
	}
	// A file changed there (another size) is read again.
	if err := os.WriteFile(to, []byte("a longer photo"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, _ := os.Stat(to)
	if got, _ := again.sha256(to, changed); got == sha {
		t.Fatal("a changed file was taken from the table")
	}
}
