package copies

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/media"
)

// encryptedCopy is a copier of a new store into an encrypted copy at a test host; the identity only a key file holds.
func encryptedCopy(t *testing.T, h *testHost) (*media.Store, *Copier, *Encrypted, *age.X25519Identity, string) {
	t.Helper()
	store, _ := newCopier(t)
	key, identity, err := NewCopyKey("hen", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	e, err := OpenEncrypted(h.peer(t, h.enroll(t, "hen-home", 1<<30), "hen"), key, dir)
	if err != nil {
		t.Fatal(err)
	}
	c := NewTo("hen", store, auth.CopyConfig{Peer: "dana", Encrypt: true}, e, time.Now)
	return store, c, e, identity, dir
}

// hostFiles is every file the host keeps for hen-home's user hen, by path there.
func hostFiles(t *testing.T, h *testHost) map[string][]byte {
	t.Helper()
	root := filepath.Join(h.host.Folder, "hen-home", "hen")
	files := map[string][]byte{}
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			files[filepath.ToSlash(rel)], _ = os.ReadFile(path)
		}
		return nil
	})
	return files
}

func TestAnEncryptedCopyHidesNamesAndBytes(t *testing.T) {
	h := newTestHost(t)
	store, c, e, identity, _ := encryptedCopy(t, h)
	addPhoto(t, store, "IMG_1.jpg", "a photo the friend mustn't see")
	addPhoto(t, store, "IMG_2.jpg", "another private photo")
	pass := c.Run(context.Background())
	if pass.Err != nil || pass.Failed != 0 || pass.Files < 4 || !c.Encrypted() || c.PeerName() != "dana" {
		t.Fatalf("the pass: %+v", pass)
	}
	at := hostFiles(t, h)
	var blobs int
	for name, data := range at {
		if strings.Contains(name, "IMG_") || strings.Contains(name, "media") || strings.Contains(name, ".json") ||
			bytes.Contains(data, []byte("photo")) || bytes.Contains(data, []byte("IMG_")) {
			t.Errorf("the friend can read %s: %.40q", name, data)
		}
		if strings.HasPrefix(name, "blobs/") {
			blobs++
		}
	}
	if blobs != e.Files() || at[manifestNames[1]] == nil {
		t.Errorf("at the host: %d blobs for %d files, manifest %v", blobs, e.Files(), at[manifestNames[1]] != nil)
	}
	// The key file's identity opens each file with plain age.
	for _, item := range c.items() {
		r, err := age.Decrypt(bytes.NewReader(at[e.key.BlobName(item.Path)]), identity)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(r)
		want, _ := os.ReadFile(c.source(item.Path))
		if !bytes.Equal(got, want) {
			t.Errorf("%s decrypts to %q", item.Path, got)
		}
	}
	// What the copy lacks is known from the manifest: nothing.
	if report, err := c.Check(context.Background(), false); err != nil || report.Originals != 0 || report.Sidecars != 0 {
		t.Fatalf("after the pass: %+v %v", report, err)
	}
	if again := c.Run(context.Background()); again.Files != 0 || again.Err != nil {
		t.Errorf("a second pass sent %+v", again)
	}
}

func TestAnEncryptedCopysManifestComesBackFromThePeer(t *testing.T) {
	h := newTestHost(t)
	store, c, e, _, dir := encryptedCopy(t, h)
	addPhoto(t, store, "IMG_1.jpg", "a photo")
	if pass := c.Run(context.Background()); pass.Err != nil {
		t.Fatal(pass.Err)
	}
	// This server set up again: its own manifest is gone, the peer's is read with the naming key.
	os.Remove(filepath.Join(dir, "manifest.json"))
	again, err := OpenEncrypted(e.Inner, e.key, dir)
	if err != nil {
		t.Fatal(err)
	}
	if again.Files() != e.Files() {
		t.Fatalf("from the peer: %d files, want %d", again.Files(), e.Files())
	}
	c2 := NewTo("hen", store, auth.CopyConfig{Peer: "dana", Encrypt: true}, again, time.Now)
	if pass := c2.Run(context.Background()); pass.Files != 0 {
		t.Errorf("sent again after the manifest came back: %+v", pass)
	}
	// Another key can't open it.
	other, _, _ := NewCopyKey("hen", time.Now())
	if _, _, err := OpenManifest(other, hostFiles(t, h)[manifestNames[1]]); err == nil {
		t.Error("another key opened the manifest")
	}
}

func TestABlobGoneFromThePeerIsSentAgain(t *testing.T) {
	h := newTestHost(t)
	store, c, e, _, _ := encryptedCopy(t, h)
	addPhoto(t, store, "IMG_1.jpg", "a photo")
	if pass := c.Run(context.Background()); pass.Err != nil {
		t.Fatal(pass.Err)
	}
	item := c.items()[0]
	os.Remove(filepath.Join(h.host.Folder, "hen-home", "hen", filepath.FromSlash(e.key.BlobName(item.Path))))
	e.Inner.listed = map[string]listing{} // the listing kept is older than the friend's change
	report, err := c.Check(context.Background(), false)
	if err != nil || report.Originals != 1 {
		t.Fatalf("with a blob gone: %+v %v", report, err)
	}
	if pass := c.Run(context.Background()); pass.Files != 1 || pass.Err != nil {
		t.Errorf("the pass after: %+v", pass)
	}
}

func TestAnEncryptedCopyCountsWhatItSends(t *testing.T) {
	h := newTestHost(t)
	store, c, _, _, _ := encryptedCopy(t, h)
	addPhoto(t, store, "IMG_1.jpg", strings.Repeat("a big photo ", 50_000))
	var counted int64
	pass := Pass{}
	c.mu.Lock()
	c.running = &Progress{}
	c.mu.Unlock()
	err := c.copy(context.Background(), c.items(), nil, false, &pass)
	c.mu.Lock()
	counted = c.running.DoneBytes
	c.mu.Unlock()
	if err != nil || counted != int64(len(strings.Repeat("a big photo ", 50_000))) {
		t.Errorf("counted %d bytes for a file of %d: %v", counted, 600_000, err)
	}
}
