package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image/color"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/media"
	"github.com/journal/server-go/search"
)

// journal-server index reads a data folder's new files with the models there, writes the caches and exits; a
// second run while one holds the lock refuses at once.
func TestTheIndexCommandReadsAndExits(t *testing.T) {
	model, _ := filepath.Abs(auth.DefaultSearchModel)
	if _, err := os.Stat(model); err != nil {
		t.Skip("no search model (go run ./scripts/bundle-onnx)")
	}
	if _, _, err := media.FindVips(""); err != nil {
		t.Skip("libvips not installed")
	}
	dir := t.TempDir()
	library, err := media.OpenLibrary(map[string]string{"hen": dir}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var shas []string
	for i, c := range []color.Color{color.RGBA{220, 20, 20, 255}, color.RGBA{20, 40, 220, 255}} {
		data := plainJPEG(t, c)
		path := filepath.Join(t.TempDir(), "p.jpg")
		os.WriteFile(path, data, 0o644)
		sum := sha256.Sum256(data)
		sha := hex.EncodeToString(sum[:])
		if _, _, err := library.Store("hen").Import(path, media.UploadRequest{Sha256: sha, Size: int64(len(data)),
			FileName: []string{"RED.jpg", "BLUE.jpg"}[i], MimeType: "image/jpeg", Kind: media.KindPhoto, UploadedBy: "t"}); err != nil {
			t.Fatal(err)
		}
		shas = append(shas, sha)
	}
	none := filepath.Join(t.TempDir(), "none")
	config := auth.AppConfig{
		Users:     []auth.UserConfig{{Username: "hen", DataFolder: dir}},
		AdminAddr: "127.0.0.1:1", AdminTokenFile: filepath.Join(t.TempDir(), "admin.token"), // no server to tell
		SearchModel: model, FaceModel: none, NudityModel: none, OCRModel: none, SimilarModel: none, AestheticModel: none,
	}.WithDefaults()

	// A run already holding the lock: refused.
	unlock, err := library.Store("hen").LockIndexes()
	if err != nil {
		t.Fatal(err)
	}
	if err := runIndex(config); !errors.Is(err, media.ErrIndexesLocked) {
		t.Fatalf("beside another run: %v", err)
	}
	unlock()

	if err := runIndex(config); err != nil {
		t.Fatal(err)
	}
	vectors, err := search.OpenVectorsReadOnly(filepath.Join(dir, ".cache", "search", filepath.Base(model)+".vectors"), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, sha := range shas {
		if !vectors.Has(sha) {
			t.Errorf("%s not read", sha[:8])
		}
	}
}
