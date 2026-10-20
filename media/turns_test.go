package media

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Two stores on one data folder stand in for two processes (the server and an import).
func TestStoresOnOneFolderTakeTurns(t *testing.T) {
	server := newFixture(t)
	importer, err := OpenStore(server.root, func() time.Time { return server.now })
	if err != nil {
		t.Fatal(err)
	}
	var learned []string
	importer.OnCatchUp(func(item Item) { learned = append(learned, item.Path) })
	_, cursor, _, _ := server.store.Changes("", 10)

	// The import places a file: the server's feed has it at once, and a phone's upload of it stores nothing.
	data := []byte("a photo from an old backup")
	source := filepath.Join(t.TempDir(), "IMG_1.jpg")
	os.WriteFile(source, data, 0o644)
	server.now = server.now.Add(time.Second)
	imported, existed, err := importer.Import(source, photoRequest(data, "IMG_1.jpg"))
	if err != nil || existed {
		t.Fatalf("import: %v %v", existed, err)
	}
	if items, _, _, _ := server.store.Changes(cursor, 10); len(items) != 1 || items[0].Path != imported.Path {
		t.Fatalf("the server's feed after the import: %v", items)
	}
	if missing := server.store.Missing([]string{shaOf(data)}); len(missing) != 0 {
		t.Errorf("missing after the import: %v", missing)
	}

	// An upload that started before the server knew finishes as the file already there.
	other := []byte("a different photo, same name")
	server.start(photoRequest(other, "IMG_1.jpg"))
	stale := photoRequest(data, "IMG_1.jpg")
	result, err := server.store.StartUpload(stale)
	if err != nil || result.Existing == nil {
		t.Fatalf("upload of the imported file: %+v %v", result, err)
	}

	// Same name, different bytes: kept side by side, never overwritten.
	uploaded, err := server.chunk(other, 0, int64(len(other)))
	if err != nil || uploaded.Item == nil || uploaded.Item.Path == imported.Path {
		t.Fatalf("upload with the same name: %+v %v", uploaded, err)
	}
	if kept, _ := os.ReadFile(filepath.Join(server.root, filepath.FromSlash(imported.Path))); !bytes.Equal(kept, data) {
		t.Error("the imported file was overwritten")
	}

	// Folders added by both stores all stay: each rewrites the sidecar after catching up with the other.
	if _, err := importer.AddFolders(shaOf(data), []string{"phone DCIM/Camera"}); err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.AddFolders(shaOf(data), []string{"phone DCIM/Flowers"}); err != nil {
		t.Fatal(err)
	}
	if _, err := importer.AddFolders(shaOf(data), []string{"Google Drive/Photos"}); err != nil {
		t.Fatal(err)
	}
	reopened, _ := OpenStore(server.root, time.Now)
	got, _ := reopened.Get(shaOf(data))
	if folders := strings.Join(got.Sidecar.Facts.Folders, "|"); folders != "phone DCIM/Camera|phone DCIM/Flowers|Google Drive/Photos" {
		t.Errorf("folders %s", folders)
	}

	// The import learns of the server's upload too.
	importer.CatchUp()
	if _, ok := importer.Get(shaOf(other)); !ok || len(learned) != 1 || learned[0] != uploaded.Item.Path {
		t.Errorf("the import didn't learn of the upload: %v", learned)
	}
}

func TestClearImportsKeepsCopiesInProgress(t *testing.T) {
	f := newFixture(t)
	os.MkdirAll(f.store.importDir(), 0o755)
	old, fresh := filepath.Join(f.store.importDir(), "old.tmp"), filepath.Join(f.store.importDir(), "fresh.tmp")
	os.WriteFile(old, nil, 0o644)
	os.WriteFile(fresh, nil, 0o644)
	os.Chtimes(old, f.now.Add(-48*time.Hour), f.now.Add(-48*time.Hour))
	os.Chtimes(fresh, f.now, f.now)
	if err := f.store.ClearImports(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("a stale copy stayed")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a copy in progress was deleted")
	}
}
