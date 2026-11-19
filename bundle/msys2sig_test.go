package bundle

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/openpgp" //nolint:staticcheck
)

func TestTheMSYS2KeysAreRead(t *testing.T) {
	keys, err := msys2Keys()
	if err != nil || len(keys) != 8 {
		t.Fatalf("%d keys, %v", len(keys), err)
	}
}

func TestPackagesNeedAPackagersSignature(t *testing.T) {
	packager, err := openpgp.NewEntity("packager", "", "packager@example.org", nil)
	if err != nil {
		t.Fatal(err)
	}
	stranger, _ := openpgp.NewEntity("stranger", "", "stranger@example.org", nil)
	keys := func() (openpgp.EntityList, error) { return openpgp.EntityList{packager}, nil }
	dir := t.TempDir()
	path := filepath.Join(dir, "libvips.pkg.tar.zst")
	if err := os.WriteFile(path, []byte("the package"), 0o644); err != nil {
		t.Fatal(err)
	}
	sign := func(by *openpgp.Entity, data string) []byte {
		var sig bytes.Buffer
		if err := openpgp.DetachSign(&sig, by, bytes.NewReader([]byte(data)), nil); err != nil {
			t.Fatal(err)
		}
		return sig.Bytes()
	}
	if err := checkSignature(path, sign(packager, "the package"), keys); err != nil {
		t.Errorf("signed by the packager: %v", err)
	}
	if err := checkSignature(path, sign(stranger, "the package"), keys); err == nil {
		t.Error("signed by someone else: taken")
	}
	if err := checkSignature(path, sign(packager, "another package"), keys); err == nil {
		t.Error("the packager's signature of another file: taken")
	}
	if err := checkSignature(path, nil, keys); err == nil {
		t.Error("no signature: taken")
	}
}
