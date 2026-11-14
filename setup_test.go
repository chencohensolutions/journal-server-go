package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/journal/server-go/auth"
)

// hashTree is every file under dir (relative path → sha256), to show setup changed nothing there.
func hashTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	sums := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		rel, _ := filepath.Rel(dir, path)
		sums[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sums
}

func runSetupIn(t *testing.T, args ...string) {
	t.Helper()
	o, err := parseSetupArgs(append(args, "--no-downloads"))
	if err != nil {
		t.Fatal(err)
	}
	if err := setup(context.Background(), o); err != nil {
		t.Fatal(err)
	}
}

func TestSetupNewServer(t *testing.T) {
	t.Chdir(t.TempDir())
	base := t.TempDir()
	dir, data := filepath.Join(base, "server"), filepath.Join(base, "journal", "hen")
	runSetupIn(t, "--dir", dir, "--user", "hen", "--data-folder", data, "--new-ca", "--address", "192.168.50.9")

	config, err := auth.LoadConfig(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Users) != 1 || config.Users[0].Username != "hen" || config.PublicURL != "https://192.168.50.9:56398" {
		t.Fatalf("config: %+v", config)
	}
	if _, err := os.Stat(data); err != nil {
		t.Errorf("data folder not made: %v", err)
	}
	// The server can load what setup made (the same call serve makes).
	t.Chdir(dir)
	if _, err := tls.LoadX509KeyPair(config.TLSCert, config.TLSKey); err != nil {
		t.Fatal(err)
	}
	if _, err := createTLSConfig(config); err != nil {
		t.Fatal(err)
	}

	// Running it again changes nothing that is there.
	before := hashTree(t, base)
	runSetupIn(t, "--dir", dir, "--user", "hen", "--data-folder", data, "--new-ca", "--address", "192.168.50.9")
	if after := hashTree(t, base); !maps.Equal(before, after) {
		t.Errorf("a second setup changed files:\nbefore %v\nafter  %v", before, after)
	}
}

func TestSetupLeavesAnExistingDataFolderAlone(t *testing.T) {
	t.Chdir(t.TempDir())
	base := t.TempDir()
	data := filepath.Join(base, "journal")
	if err := os.MkdirAll(filepath.Join(data, "media", "2026"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "media", "2026", "a.jpg.json"), []byte(`{"format":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	before := hashTree(t, data)
	runSetupIn(t, "--dir", filepath.Join(base, "server"), "--user", "hen", "--data-folder", data, "--new-ca")
	if after := hashTree(t, data); !maps.Equal(before, after) {
		t.Errorf("setup wrote into the data folder: %v", after)
	}
}

func TestSetupMoveFromOnlyReadsTheOldServer(t *testing.T) {
	t.Chdir(t.TempDir())
	base := t.TempDir()
	old := filepath.Join(base, "old")
	runSetupIn(t, "--dir", old, "--user", "hen", "--data-folder", filepath.Join(base, "journal"), "--new-ca")
	for name, content := range map[string]string{"devices.json": `{"devices":[]}`, "admin.token": "secret"} {
		if err := os.WriteFile(filepath.Join(old, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := hashTree(t, base)

	dir := filepath.Join(base, "new")
	runSetupIn(t, "--dir", dir, "--move-from", old)

	after := hashTree(t, base)
	for path, sum := range before {
		if after[path] != sum {
			t.Errorf("%s changed", path)
		}
	}
	for _, name := range []string{"config.json", "devices.json", "certs/ca.pem", "certs/ca-key.pem", "certs/server.pem", "certs/server-key.pem"} {
		want := before[filepath.Join("old", filepath.FromSlash(name))]
		if got := after[filepath.Join("new", filepath.FromSlash(name))]; got != want || got == "" {
			t.Errorf("%s not copied as it was", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "admin.token")); err == nil {
		t.Error("admin.token was copied")
	}

	// Moving into the old folder itself is refused.
	o, err := parseSetupArgs([]string{"--dir", old, "--move-from", old, "--no-downloads"})
	if err != nil {
		t.Fatal(err)
	}
	if err := setup(context.Background(), o); err == nil {
		t.Error("setup moved a server into its own folder")
	}
}

func TestSetupReissueKeepsTheOldServerCertificate(t *testing.T) {
	t.Chdir(t.TempDir())
	base := t.TempDir()
	dir := filepath.Join(base, "server")
	runSetupIn(t, "--dir", dir, "--user", "hen", "--data-folder", filepath.Join(base, "journal"), "--new-ca")
	old, err := os.ReadFile(filepath.Join(dir, "certs", "server.pem"))
	if err != nil {
		t.Fatal(err)
	}
	runSetupIn(t, "--dir", dir, "--address", "10.1.2.3", "--reissue-server-cert")
	backup, err := os.ReadFile(filepath.Join(dir, "certs", "server.pem.bak"))
	if err != nil || string(backup) != string(old) {
		t.Fatalf("old certificate not kept as .bak: %v", err)
	}
}

func TestSetupRefusesUnclearRequests(t *testing.T) {
	for _, args := range [][]string{
		{"--move-from", "x", "--user", "hen"},
		{"--import-ca", "x", "--new-ca"},
		{"stray"},
	} {
		if _, err := parseSetupArgs(args); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
}

// A new server gets its own CA without being asked; a server that has one never gets another by itself.
func TestSetupMakesEachServerItsOwnCA(t *testing.T) {
	t.Chdir(t.TempDir())
	base := t.TempDir()
	dir := filepath.Join(base, "server")
	runSetupIn(t, "--dir", dir, "--user", "hen", "--data-folder", filepath.Join(base, "journal"))
	for _, f := range []string{"ca.pem", "ca-key.pem", "server.pem"} {
		if _, err := os.Stat(filepath.Join(dir, "certs", f)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	other := filepath.Join(base, "other")
	runSetupIn(t, "--dir", other, "--user", "dana", "--data-folder", filepath.Join(base, "journal2"))
	mine, _ := os.ReadFile(filepath.Join(dir, "certs", "ca.pem"))
	theirs, _ := os.ReadFile(filepath.Join(other, "certs", "ca.pem"))
	if string(mine) == string(theirs) {
		t.Fatal("two servers have the same CA")
	}

	// The CA lost from a server with devices: setup (the installer's upgrade) refuses rather than make another.
	if err := os.Remove(filepath.Join(dir, "certs", "ca.pem")); err != nil {
		t.Fatal(err)
	}
	o, err := parseSetupArgs([]string{"--dir", dir, "--no-downloads"})
	if err != nil {
		t.Fatal(err)
	}
	if err := setup(context.Background(), o); err == nil || !strings.Contains(err.Error(), "no CA") {
		t.Fatalf("setup without the CA: %v", err)
	}
}
