package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/journal/server-go/media"
)

// The sidecar says where the source was found and where the run moved it, so the import can be undone.
func TestImportIntoRecordsWhereTheSourceWent(t *testing.T) {
	store, err := media.OpenStore(t.TempDir(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	imported := t.TempDir()
	root := filepath.Join(t.TempDir(), "Google Photos")
	path := filepath.Join(root, "Trip", "20250101_120000.jpg")
	data := []byte("a photo from Takeout")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	taken := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	im := &importer{store: store, importedFolder: imported, runFolder: filepath.Join(imported, "2026-10-21-09-30-00")}

	item, _, err := im.importInto(path, path, im.movedTo([]string{root}, path), hex.EncodeToString(sum[:]), int64(len(data)),
		facts{kind: media.KindPhoto, mime: "image/jpeg", takenAt: &taken}, []string{"Google Photos/Trip"})
	if err != nil {
		t.Fatal(err)
	}
	facts := item.Sidecar.Facts
	if facts.ImportedFrom == nil || *facts.ImportedFrom != path {
		t.Errorf("importedFrom %v, want %s", facts.ImportedFrom, path)
	}
	if want := "2026-10-21-09-30-00/Google Photos/Trip/20250101_120000.jpg"; facts.ImportedTo == nil || *facts.ImportedTo != want {
		t.Errorf("importedTo %v, want %s", facts.ImportedTo, want)
	}
}
