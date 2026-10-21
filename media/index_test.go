package media

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIndexCacheReadsOnlyChangedFolders(t *testing.T) {
	f := newFixture(t)
	item := f.upload([]byte("photo"), "IMG.jpg")
	f.reopen() // writes the cache
	if _, err := os.Stat(f.store.indexPath()); err != nil {
		t.Fatal(err)
	}

	// A sidecar changed in place, with its folder's time put back, is not read again: the cache answers.
	sidecar := filepath.Join(f.root, filepath.FromSlash(item.Path)) + ".json"
	folder := filepath.Dir(sidecar)
	info, err := os.Stat(folder)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecar, []byte(strings.Replace(string(data), "IMG.jpg", "NEW.jpg", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(folder, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	f.reopen()
	if got, _ := f.store.Get(item.Sha256()); got.Sidecar.Facts.FileName != "IMG.jpg" {
		t.Fatalf("from the cache: %q", got.Sidecar.Facts.FileName)
	}

	// Once the folder's time changes, it is read again.
	later := info.ModTime().Add(time.Minute)
	if err := os.Chtimes(folder, later, later); err != nil {
		t.Fatal(err)
	}
	f.reopen()
	if got, _ := f.store.Get(item.Sha256()); got.Sidecar.Facts.FileName != "NEW.jpg" {
		t.Fatalf("read again: %q", got.Sidecar.Facts.FileName)
	}

	// A file placed and a file removed since the cache was written are both seen.
	f.now = f.now.Add(time.Hour)
	second := f.upload([]byte("second photo"), "IMG2.jpg")
	f.reopen()
	if _, ok := f.store.Get(second.Sha256()); !ok {
		t.Fatal("new file missing after reopening")
	}
	original := filepath.Join(f.root, filepath.FromSlash(item.Path))
	if err := os.Remove(original); err != nil {
		t.Fatal(err)
	}
	f.reopen()
	if _, ok := f.store.Get(item.Sha256()); ok {
		t.Fatal("a file whose original is gone is still indexed")
	}
	if _, ok := f.store.Get(second.Sha256()); !ok {
		t.Fatal("the other file went missing")
	}
}

func TestIndexCacheOfAnotherFormatIsIgnored(t *testing.T) {
	f := newFixture(t)
	item := f.upload([]byte("photo"), "IMG.jpg")
	if err := os.WriteFile(f.store.indexPath(), []byte(`{"format": 999, "folders": {}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	f.reopen()
	if _, ok := f.store.Get(item.Sha256()); !ok {
		t.Fatal("not indexed")
	}
}
