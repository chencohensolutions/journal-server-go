package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/journal/server-go/media"
)

func TestFixCorrectsSourceByName(t *testing.T) {
	root, disk := t.TempDir(), t.TempDir()
	store, err := media.OpenStore(root, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	add := func(name, folder, source string) string {
		data := []byte(name)
		sum := sha256.Sum256(data)
		sha := hex.EncodeToString(sum[:])
		path := filepath.Join(disk, folder, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		from := filepath.ToSlash(path)
		_, _, err := store.Import(path, media.UploadRequest{
			Sha256: sha, Size: int64(len(data)), FileName: name, MimeType: "image/jpeg", Kind: media.KindPhoto,
			Source: &source, UploadedBy: "import", ImportedFrom: &from, Folders: []string{folder},
		})
		if err != nil {
			t.Fatal(err)
		}
		return sha
	}
	named := add("IMG-20230115-WA0012.jpg", "Google Photos/2023", media.SourceOther)
	camera := add("IMG_20230115_101010.jpg", "DCIM/Camera", media.SourceCamera)
	add("IMG-20230116-WA0001.jpg", "WhatsApp/Media/WhatsApp Images", media.SourceWhatsApp)
	add("x.jpg", "Backup/WhatsApp copies", media.SourceOther)

	r, err := fix(store, true)
	if err != nil || r.fixed != 1 || len(r.folderOnly) != 1 {
		t.Fatalf("dry run: %+v %v", r, err)
	}
	if got, _ := store.Get(named); *got.Sidecar.Facts.Source != media.SourceOther {
		t.Fatal("a dry run changed the sidecar")
	}

	if r, err = fix(store, false); err != nil || r.fixed != 1 || r.was[media.SourceOther] != 1 {
		t.Fatalf("fix: %+v %v", r, err)
	}
	if got, _ := store.Get(named); *got.Sidecar.Facts.Source != media.SourceWhatsApp || got.Sidecar.Facts.CorrectedAt == nil {
		t.Fatalf("not corrected: %+v", got.Sidecar.Facts)
	}
	if got, _ := store.Get(camera); *got.Sidecar.Facts.Source != media.SourceCamera {
		t.Fatal("a camera photo changed")
	}
	if r, _ = fix(store, false); r.fixed != 0 {
		t.Fatalf("second run fixed %d", r.fixed)
	}
}

func TestFixScreenshotsSetsTheirApp(t *testing.T) {
	// A screenshot placed before the server read the app from its name: source as its folder said, no app.
	root := t.TempDir()
	dir := filepath.Join(root, "media", "2024", "2024-03")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "Screenshot_20240301_030715_Bumble.jpg"
	data := []byte(name)
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	sidecar := `{"format":1,"sha256":"` + sha + `","facts":{"fileName":"` + name + `","kind":"photo","mimeType":"image/jpeg",` +
		`"size":` + strconv.Itoa(len(data)) + `,"takenAt":"2024-03-01T03:07:15+02:00","source":"other","uploadedAt":"2026-10-20T17:50:53Z","uploadedBy":"import"}}`
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(sidecar), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := media.OpenStore(root, time.Now)
	if err != nil {
		t.Fatal(err)
	}

	if apps, err := fixScreenshots(store, true); err != nil || apps["Bumble"] != 1 {
		t.Fatalf("dry run: %v %v", apps, err)
	}
	if got, _ := store.Get(sha); got.Sidecar.Facts.App != nil {
		t.Fatal("a dry run changed the sidecar")
	}
	if apps, err := fixScreenshots(store, false); err != nil || apps["Bumble"] != 1 {
		t.Fatalf("fix: %v %v", apps, err)
	}
	if got, _ := store.Get(sha); *got.Sidecar.Facts.Source != media.SourceScreenshots || got.Sidecar.Facts.App == nil || *got.Sidecar.Facts.App != "Bumble" {
		t.Fatalf("not corrected: %+v", got.Sidecar.Facts)
	}
	if apps, _ := fixScreenshots(store, false); len(apps) != 0 {
		t.Fatalf("second run: %v", apps)
	}
}
