package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/journal/server-go/media"
)

// TestDeleteDuplicates: with --delete-duplicates a source file the data folder already has is deleted, only when
// both have the same bytes; the data folder is never touched, and a dry run deletes nothing.
func TestDeleteDuplicates(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	store, err := media.OpenStore(data, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	bytes := []byte("a photo already in the journal")
	sha := shaOf(t, bytes)
	original := write(t, filepath.Join(dir, "first", "IMG_1.jpg"), bytes)
	taken := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	item, _, err := store.Import(original, media.UploadRequest{Sha256: sha, Size: int64(len(bytes)), FileName: "IMG_1.jpg", MimeType: "image/jpeg", Kind: media.KindPhoto, TakenAt: &taken, UploadedBy: "import"})
	if err != nil {
		t.Fatal(err)
	}
	inData := filepath.Join(data, filepath.FromSlash(item.Path))

	root := filepath.Join(dir, "source")
	importer := func(dryRun bool) *importer {
		return &importer{store: store, dataFolder: data, runFolder: filepath.Join(dir, "imported", "run"), roots: []string{root},
			deleteDups: true, dryRun: dryRun, verified: map[string]bool{}}
	}

	// A dry run only counts.
	copy1 := write(t, filepath.Join(root, "a", "IMG_1.jpg"), bytes)
	dry := importer(true)
	dry.done(copy1, sha, resultDuplicate)
	if _, err := os.Stat(copy1); err != nil || dry.counts.deleted != 1 {
		t.Fatalf("dry run: %v, %d counted", err, dry.counts.deleted)
	}

	// The copy is deleted; the data folder's original stays.
	im := importer(false)
	im.done(copy1, sha, resultDuplicate)
	if _, err := os.Stat(copy1); !os.IsNotExist(err) {
		t.Fatalf("the duplicate is still there: %v", err)
	}
	if got, err := os.ReadFile(inData); err != nil || string(got) != string(bytes) {
		t.Fatalf("the data folder's original: %q %v", got, err)
	}
	if im.counts.deleted != 1 || len(im.deleted) != 1 {
		t.Fatalf("counted %d, listed %v", im.counts.deleted, im.deleted)
	}

	// A file changed since it was hashed isn't deleted.
	changed := write(t, filepath.Join(root, "b", "IMG_1.jpg"), []byte("changed meanwhile"))
	im.done(changed, sha, resultDuplicate)
	if _, err := os.Stat(changed); err != nil {
		t.Fatalf("a changed file was deleted: %v", err)
	}

	// An original damaged in the data folder keeps every copy: it is moved to the imported folder as before.
	if err := os.WriteFile(inData, []byte("damaged"), 0o644); err != nil {
		t.Fatal(err)
	}
	copy2 := write(t, filepath.Join(root, "c", "IMG_1.jpg"), bytes)
	fresh := importer(false)
	fresh.done(copy2, sha, resultDuplicate)
	if _, err := os.Stat(copy2); !os.IsNotExist(err) {
		t.Fatalf("not moved away: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "imported", "run", "source", "c", "IMG_1.jpg")); err != nil || fresh.counts.deleted != 0 {
		t.Fatalf("not kept in the imported folder: %v (%d deleted)", err, fresh.counts.deleted)
	}
}

func write(t *testing.T, path string, data []byte) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func shaOf(t *testing.T, data []byte) string {
	t.Helper()
	path := write(t, filepath.Join(t.TempDir(), "x"), data)
	sum, err := media.HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}
