package media

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportCopiesOnceAndKeepsTheSource(t *testing.T) {
	f := newFixture(t)
	data := []byte("a photo from an old phone backup")
	source := filepath.Join(t.TempDir(), "IMG_1234.jpg")
	if err := os.WriteFile(source, data, 0o644); err != nil {
		t.Fatal(err)
	}
	req := photoRequest(data, "IMG_1234.jpg")
	req.UploadedBy = "import"
	req.ImportedFrom = &source

	item, existed, err := f.store.Import(source, req)
	if err != nil || existed {
		t.Fatalf("Import: %+v %v %v", item, existed, err)
	}
	if item.Path != "media/2026/2026-10/IMG_1234.jpg" {
		t.Errorf("path %s", item.Path)
	}
	if kept, err := os.ReadFile(source); err != nil || string(kept) != string(data) {
		t.Errorf("source changed: %q %v", kept, err)
	}
	var sidecar Sidecar
	raw, _ := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(item.Path)) + ".json")
	if err := json.Unmarshal(raw, &sidecar); err != nil || sidecar.Facts.ImportedFrom == nil || *sidecar.Facts.ImportedFrom != source {
		t.Errorf("sidecar %s: %v", raw, err)
	}

	again, existed, err := f.store.Import(source, req)
	if err != nil || !existed || again.Path != item.Path {
		t.Errorf("second Import: %+v %v %v", again, existed, err)
	}
	f.reopen()
	if _, ok := f.store.Get(req.Sha256); !ok {
		t.Error("imported file not found after reopening")
	}
}

func TestImportRefusesACopyWithAnotherHash(t *testing.T) {
	f := newFixture(t)
	source := filepath.Join(t.TempDir(), "IMG_1.jpg")
	if err := os.WriteFile(source, []byte("changed after it was hashed"), 0o644); err != nil {
		t.Fatal(err)
	}
	req := photoRequest([]byte("what it was when hashed"), "IMG_1.jpg")
	req.Size = int64(len("changed after it was hashed"))
	if _, _, err := f.store.Import(source, req); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("Import: %v", err)
	}
	if _, ok := f.store.Get(req.Sha256); ok {
		t.Error("stored anyway")
	}
	if entries, _ := os.ReadDir(f.store.importDir()); len(entries) != 0 {
		t.Errorf("copy left behind: %v", entries)
	}
}

func TestAddFoldersRewritesOnlyTheSidecar(t *testing.T) {
	f := newFixture(t)
	data := []byte("a photo kept in two albums")
	req := photoRequest(data, "IMG_7.jpg")
	req.Folders = []string{"phone DCIM/Camera"}
	source := filepath.Join(t.TempDir(), "IMG_7.jpg")
	os.WriteFile(source, data, 0o644)
	item, _, err := f.store.Import(source, req)
	if err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(f.root, filepath.FromSlash(item.Path))

	for _, folders := range [][]string{{"phone DCIM/Flowers"}, {"PHONE DCIM/flowers", "phone DCIM/Camera"}} {
		if _, err := f.store.AddFolders(req.Sha256, folders); err != nil {
			t.Fatal(err)
		}
	}
	f.reopen()
	got, _ := f.store.Get(req.Sha256)
	if want := []string{"phone DCIM/Camera", "phone DCIM/Flowers"}; strings.Join(got.Sidecar.Facts.Folders, "|") != strings.Join(want, "|") {
		t.Errorf("folders %v, want %v", got.Sidecar.Facts.Folders, want)
	}
	if kept, _ := os.ReadFile(original); string(kept) != string(data) {
		t.Error("the original changed")
	}
	if _, err := f.store.AddFolders(shaOf([]byte("unknown")), []string{"x"}); err == nil {
		t.Error("unknown file accepted")
	}
}
