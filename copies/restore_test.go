package copies

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/journal/server-go/auth"
)

// keyFileOf is a key file as copy-key new writes it.
func keyFileOf(t *testing.T, key CopyKey, identity *age.X25519Identity) KeyFile {
	t.Helper()
	text := fmt.Sprintf("# Journal copy key of hen\n# journal-user: %s\n# journal-naming-key: %s\n%s\n", key.User,
		base64.StdEncoding.EncodeToString(key.NamingKey), identity)
	kf, err := ReadKeyFile([]byte(text), nil)
	if err != nil {
		t.Fatal(err)
	}
	return kf
}

// sameMedia fails unless restored holds the data folder's originals, sidecars and root files, byte for byte.
func sameMedia(t *testing.T, root, restored string) {
	t.Helper()
	want, got := tree(t, filepath.Join(root, "media")), tree(t, filepath.Join(restored, "media"))
	if len(want) == 0 || len(got) != len(want) {
		t.Fatalf("restored %d files of %d", len(got), len(want))
	}
	for path, file := range want {
		w, _, _ := strings.Cut(file, "@")
		g, _, _ := strings.Cut(got[path], "@")
		if g != w {
			t.Errorf("%s came back as %q", path, g)
		}
	}
}

func TestAPlainCopyComesBack(t *testing.T) {
	store, c := newCopier(t)
	addPhoto(t, store, "IMG_1.jpg", "a photo")
	addPhoto(t, store, "IMG_2.jpg", "another photo")
	os.WriteFile(filepath.Join(store.Root(), "people.json"), []byte(`{"v":1}`), 0o644)
	if pass := c.Run(context.Background()); pass.Err != nil {
		t.Fatal(pass.Err)
	}
	to := filepath.Join(t.TempDir(), "restored")
	result, err := Restore(context.Background(), Folder(c.Folder), nil, to, nil)
	if err != nil || result.Failed != 0 || result.Encrypted || result.Done != result.Files {
		t.Fatalf("restore: %+v %v", result, err)
	}
	sameMedia(t, store.Root(), to)
	if data, _ := os.ReadFile(filepath.Join(to, "people.json")); string(data) != `{"v":1}` {
		t.Errorf("people.json came back as %q", data)
	}
	if _, err := os.Stat(filepath.Join(to, restoreMark)); err == nil {
		t.Error("the restore's mark stayed after it ended")
	}
	// Into a folder that isn't empty: refused.
	if _, err := Restore(context.Background(), Folder(c.Folder), nil, to, nil); err == nil {
		t.Error("a restore filled a folder that wasn't empty")
	}
}

// The owner's server is lost; the key file and the friend's server bring everything back, over the network or from
// the friend's folder handed over on a disk. A damaged blob is listed, and running again goes on.
func TestAnEncryptedCopyComesBackWithTheKeyFile(t *testing.T) {
	h := newTestHost(t)
	store, c, e, identity, _ := encryptedCopy(t, h)
	addPhoto(t, store, "IMG_1.jpg", "a photo the friend mustn't see")
	addPhoto(t, store, "IMG_2.jpg", strings.Repeat("a bigger photo ", 10_000))
	if pass := c.Run(context.Background()); pass.Err != nil || pass.Failed != 0 {
		t.Fatalf("the pass: %+v", pass)
	}
	kf := keyFileOf(t, e.key, identity)

	// Over the network, with a peer client that knows nothing of the copy (a new server).
	fresh := h.peer(t, e.Inner.certificate(t), "hen")
	if _, err := Restore(context.Background(), fresh, nil, filepath.Join(t.TempDir(), "x"), nil); err == nil {
		t.Error("an encrypted copy restored without the key file")
	}
	other, otherIdentity, _ := NewCopyKey("hen", time.Now())
	if _, err := Restore(context.Background(), fresh, ptr(keyFileOf(t, other, otherIdentity)), filepath.Join(t.TempDir(), "y"), nil); err == nil {
		t.Error("restored with another key file")
	}
	to := filepath.Join(t.TempDir(), "restored")
	result, err := Restore(context.Background(), fresh, &kf, to, nil)
	if err != nil || result.Failed != 0 || !result.Encrypted {
		t.Fatalf("restore: %+v %v", result, err)
	}
	sameMedia(t, store.Root(), to)

	// From the friend's folder on a disk, with one blob damaged there.
	handed := filepath.Join(h.host.Folder, "hen-home", "hen")
	item := c.items()[0]
	blob := filepath.Join(handed, filepath.FromSlash(e.key.BlobName(item.Path)))
	good, _ := os.ReadFile(blob)
	bad := bytes.Clone(good)
	bad[len(bad)-1] ^= 1
	os.WriteFile(blob, bad, 0o644)
	defer func(tries int) { RestoreTries = tries }(RestoreTries)
	RestoreTries = 1
	to = filepath.Join(t.TempDir(), "from-disk")
	result, err = Restore(context.Background(), Folder(handed), &kf, to, nil)
	if err != nil || result.Failed != 1 || result.FailedFiles[item.Path] == "" {
		t.Fatalf("with a damaged blob: %+v %v", result, err)
	}
	// Fixed (the friend found a good copy): running again brings only that one back.
	os.WriteFile(blob, good, 0o644)
	result, err = Restore(context.Background(), Folder(handed), &kf, to, nil)
	if err != nil || result.Failed != 0 || result.Done != 1 || result.Skipped != result.Files-1 {
		t.Fatalf("running again: %+v %v", result, err)
	}
	sameMedia(t, store.Root(), to)
}

func ptr[T any](v T) *T { return &v }

func TestAKeyFileIsReadOrRefused(t *testing.T) {
	key, identity, _ := NewCopyKey("hen", time.Now())
	kf := keyFileOf(t, key, identity)
	if kf.Key.User != "hen" || !bytes.Equal(kf.Key.NamingKey, key.NamingKey) {
		t.Errorf("read back: %+v", kf.Key)
	}
	for _, bad := range []string{identity.String() + "\n", "# journal-user: hen\n# journal-naming-key: AAAA\n" + identity.String() + "\n", "not a key\n"} {
		if _, err := ReadKeyFile([]byte(bad), nil); err == nil {
			t.Errorf("read %q", bad)
		}
	}
	_ = auth.CopyConfig{}
}
