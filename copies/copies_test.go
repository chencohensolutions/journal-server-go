package copies

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/media"
)

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

var taken = time.Date(2026, 10, 17, 15, 52, 22, 0, time.UTC)

// addPhoto places a photo in store as an import would, and returns its sha256.
func addPhoto(t *testing.T, store *media.Store, name, content string) string {
	t.Helper()
	data := []byte(content)
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	source := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(source, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := store.Import(source, media.UploadRequest{Sha256: sha, Size: int64(len(data)), FileName: name,
		MimeType: "image/jpeg", Kind: media.KindPhoto, TakenAt: &taken, UploadedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

// tree is every file under root (relative, forward slashes) with its bytes and modified time.
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, _ := d.Info()
		rel, _ := filepath.Rel(root, path)
		files[filepath.ToSlash(rel)] = string(data) + "@" + info.ModTime().String()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func newCopier(t *testing.T) (*media.Store, *Copier) {
	t.Helper()
	store, err := media.OpenStore(t.TempDir(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return store, New("hen", store, auth.CopyConfig{Folder: t.TempDir(), At: "03:30"}, time.Now)
}

func TestPassCopiesTruthFilesAndLeavesTheDataFolderAlone(t *testing.T) {
	store, c := newCopier(t)
	addPhoto(t, store, "IMG_1.jpg", "first photo")
	addPhoto(t, store, "IMG_2.jpg", "second photo")
	store.SetPeople([]media.Person{})

	report, err := c.Check(context.Background(), true)
	if err != nil || report.Originals != 2 || report.Sidecars != 3 || report.OriginalBytes != int64(len("first photo")+len("second photo")) {
		t.Fatalf("before a pass: %+v %v", report, err)
	}
	before := tree(t, store.Root())
	pass := c.Run(context.Background())
	if pass.Err != nil || pass.Files != 5 || pass.Failed != 0 {
		t.Fatalf("pass: %+v", pass)
	}
	if after := tree(t, store.Root()); !equalTrees(before, after) {
		t.Errorf("the pass changed the data folder")
	}

	// The copy has the originals, their sidecars and people.json, byte for byte, and no .cache or temp files.
	copied := tree(t, c.Folder)
	var names []string
	for path := range copied {
		names = append(names, path)
		source, err := os.ReadFile(filepath.Join(store.Root(), filepath.FromSlash(path)))
		if err != nil || !strings.HasPrefix(copied[path], string(source)+"@") {
			t.Errorf("%s differs from the data folder's: %v", path, err)
		}
	}
	sort.Strings(names)
	want := "media/2026/2026-10/IMG_1.jpg media/2026/2026-10/IMG_1.jpg.json media/2026/2026-10/IMG_2.jpg " +
		"media/2026/2026-10/IMG_2.jpg.json people.json"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("the copy holds %s", got)
	}

	// Nothing changed: the next pass copies nothing, and the report says the copy lacks nothing.
	if pass := c.Run(context.Background()); pass.Files != 0 || pass.Err != nil {
		t.Errorf("a second pass copied %+v", pass)
	}
	if report, _ := c.Check(context.Background(), false); report.Originals != 0 || report.Sidecars != 0 {
		t.Errorf("after a pass: %+v", report)
	}
}

func TestPassCopiesChangedSidecarsAndKeepsWhatTheDataFolderLost(t *testing.T) {
	store, c := newCopier(t)
	first := addPhoto(t, store, "IMG_1.jpg", "first photo")
	addPhoto(t, store, "IMG_2.jpg", "second photo")
	c.Run(context.Background())

	// A sidecar rewritten: only it is copied.
	if _, err := store.SetPrivate([]string{first}, true, "Pixel"); err != nil {
		t.Fatal(err)
	}
	report, _ := c.Check(context.Background(), true)
	if report.Originals != 0 || report.Sidecars != 1 || len(report.Paths) != 1 || report.Paths[0] != "media/2026/2026-10/IMG_1.jpg.json" {
		t.Fatalf("after setPrivate: %+v", report)
	}
	if pass := c.Run(context.Background()); pass.Files != 1 {
		t.Errorf("after setPrivate the pass copied %d files", pass.Files)
	}
	copiedSidecar, _ := os.ReadFile(filepath.Join(c.Folder, "media", "2026", "2026-10", "IMG_1.jpg.json"))
	if !bytes.Contains(copiedSidecar, []byte(`"private": true`)) {
		t.Errorf("the copy's sidecar: %s", copiedSidecar)
	}

	// A new file is counted, then copied; an original the copy has but the data folder doesn't stays and is counted.
	addPhoto(t, store, "IMG_3.jpg", "third photo")
	stray := filepath.Join(c.Folder, "media", "2025", "2025-01", "OLD.jpg")
	os.MkdirAll(filepath.Dir(stray), 0o755)
	os.WriteFile(stray, []byte("gone from the server"), 0o644)
	if report, _ := c.Check(context.Background(), false); report.Originals != 1 || report.InCopyOnly != 1 {
		t.Errorf("with a new file and a stray one: %+v", report)
	}
	c.Run(context.Background())
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("the copy lost a file: %v", err)
	}
}

func TestPassStopsWhenTheFolderIsGone(t *testing.T) {
	store, c := newCopier(t)
	addPhoto(t, store, "IMG_1.jpg", "first photo")
	c.Folder = filepath.Join(c.Folder, "unplugged")
	if pass := c.Run(context.Background()); pass.Err == nil || pass.Files != 0 {
		t.Errorf("a pass into a missing folder: %+v", pass)
	}
	if _, err := os.Stat(c.Folder); err == nil {
		t.Error("the pass made the missing folder")
	}
	if _, last := c.Status(); last == nil || last.Err == nil {
		t.Errorf("status after it: %+v", last)
	}
	if _, err := c.Check(context.Background(), false); err == nil {
		t.Error("a report on a missing folder has no error")
	}
}

func TestRealtimeCopiesWhatATurnWrote(t *testing.T) {
	RealtimeDelay = 10 * time.Millisecond
	store, c := newCopier(t)
	store.OnWrite(c.Changed)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.RunRealtime(ctx)

	sha := addPhoto(t, store, "IMG_1.jpg", "first photo")
	waitFor(t, filepath.Join(c.Folder, "media", "2026", "2026-10", "IMG_1.jpg.json"))
	if _, err := os.Stat(filepath.Join(c.Folder, "media", "2026", "2026-10", "IMG_1.jpg")); err != nil {
		t.Error(err)
	}

	// settings.json, written in a turn of its own, comes too.
	if err := store.SetHidden(media.Hidden{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, filepath.Join(c.Folder, "settings.json"))

	// Another process's write (another store on the folder) arrives when the server's store catches up.
	other, err := media.OpenStore(store.Root(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	other.SetPrivate([]string{sha}, true, "script")
	store.CatchUp()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, _ := os.ReadFile(filepath.Join(c.Folder, "media", "2026", "2026-10", "IMG_1.jpg.json"))
		if bytes.Contains(data, []byte(`"private": true`)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the other process's change wasn't copied")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSameDisk(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "a"), 0o755)
	os.Mkdir(filepath.Join(dir, "b"), 0o755)
	if same, err := sameDisk(filepath.Join(dir, "a"), filepath.Join(dir, "b")); err != nil || !same {
		t.Errorf("two folders in one temp folder: %v %v", same, err)
	}
}

func waitFor(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never came", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func equalTrees(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
