package copies

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/auth"
)

// quick is a derivation cheap enough for tests.
func quick() Derivation {
	d := NewDerivation()
	d.MemoryKiB, d.Time, d.Threads = 1024, 1, 1
	return d
}

func TestAKeyFromAPassphraseIsTheSameEachTime(t *testing.T) {
	d := quick()
	a, ida, err := DeriveKey("hen", "six random words make a passphrase", d, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, idb, _ := DeriveKey("hen", "six random words make a passphrase", d, time.Now())
	if a.Recipient != b.Recipient || string(a.NamingKey) != string(b.NamingKey) || ida.String() != idb.String() {
		t.Error("the same passphrase gave two keys")
	}
	if !strings.HasPrefix(ida.String(), "AGE-SECRET-KEY-1") || !strings.HasPrefix(a.Recipient, "age1") {
		t.Errorf("not an age key: %s %s", ida, a.Recipient)
	}
	other, _, _ := DeriveKey("hen", "six random words make a passphrasE", d, time.Now())
	salted, _, _ := DeriveKey("hen", "six random words make a passphrase", quick(), time.Now())
	if other.Recipient == a.Recipient || salted.Recipient == a.Recipient {
		t.Error("another passphrase or salt gave the same key")
	}
	for p, weak := range map[string]bool{"short": true, "three tiny words": true, "correct horse battery staple": false,
		"averyveryverylongpassword!": false} {
		if (WeakPassphrase(p) != "") != weak {
			t.Errorf("%q weak: %v", p, !weak)
		}
	}
}

func TestAnEncryptedCopyComesBackWithItsPassphrase(t *testing.T) {
	h := newTestHost(t)
	store, _ := newCopier(t)
	addPhoto(t, store, "IMG_1.jpg", "a photo")
	const passphrase = "six random words make a passphrase"
	key, _, err := DeriveKey("hen", passphrase, quick(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e, err := OpenEncrypted(h.peer(t, h.enroll(t, "hen-home", 1<<30), "hen"), key, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := NewTo("hen", store, auth.CopyConfig{Peer: "dana", Encrypt: true}, e, time.Now)
	if pass := c.Run(context.Background()); pass.Err != nil {
		t.Fatal(pass.Err)
	}
	handed := Folder(filepath.Join(h.host.Folder, "hen-home", "hen"))
	wrong, err := KeyFromPassphrase(handed, "hen", "not the passphrase at all, sorry")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), handed, &wrong, filepath.Join(t.TempDir(), "x"), nil); err == nil {
		t.Error("restored with the wrong passphrase")
	}
	kf, err := KeyFromPassphrase(handed, "hen", passphrase)
	if err != nil {
		t.Fatal(err)
	}
	to := filepath.Join(t.TempDir(), "restored")
	if result, err := Restore(context.Background(), handed, &kf, to, nil); err != nil || result.Failed != 0 {
		t.Fatalf("restore: %+v %v", result, err)
	}
	sameMedia(t, store.Root(), to)
}
