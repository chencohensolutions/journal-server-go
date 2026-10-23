package nudity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/media"
)

// importPhotos puts n JPEGs (gradients, so that libvips can read them) into a new library's data folder of hen.
func importPhotos(t *testing.T, n int) (*media.Library, []string) {
	t.Helper()
	root := t.TempDir()
	library, err := media.OpenLibrary(map[string]string{"hen": root}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var shas []string
	for i := range n {
		img := image.NewRGBA(image.Rect(0, 0, 400, 300))
		for y := range 300 {
			for x := range 400 {
				img.Set(x, y, color.RGBA{R: uint8(x + i), G: uint8(y), B: 90, A: 255})
			}
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, nil); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "photo.jpg")
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(buf.Bytes())
		sha := hex.EncodeToString(sum[:])
		if _, _, err := library.Store("hen").Import(path, media.UploadRequest{Sha256: sha, Size: int64(buf.Len()),
			FileName: fmt.Sprintf("IMG_%d.jpg", i), MimeType: "image/jpeg", Kind: media.KindPhoto, UploadedBy: "test"}); err != nil {
			t.Fatal(err)
		}
		shas = append(shas, sha)
	}
	return library, shas
}

// Every file that can't be scanned is listed in .cache/nudity/<model>.failed.log, also when the reason (no libvips)
// is not logged file by file, and it is tried again only after a restart.
func TestFilesThatCantBeScannedAreListed(t *testing.T) {
	library, shas := importPhotos(t, 2)
	s := NewService(&Model{}, library, media.NewPreviews("", ""))
	failed := map[string]bool{}
	s.scanNew(context.Background(), "hen", failed)
	s.scanNew(context.Background(), "hen", failed)

	store := library.Store("hen")
	data, err := os.ReadFile(filepath.Join(store.Root(), ".cache", "nudity", ModelName+".failed.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("listed:\n%s", data)
	}
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if item, ok := store.Get(fields[1]); len(fields) != 4 || !ok || fields[2] != item.Path || fields[3] == "" {
			t.Errorf("line %q (of %v)", line, shas)
		}
	}
	if todo := ToScan(store, false, nil); len(todo) != 2 {
		t.Fatalf("still to scan: %d", len(todo))
	}
}

// A scan writes its answer into each sidecar, and the next scan has nothing to do; --rescan does it all again.
func TestScanWritesTheScore(t *testing.T) {
	m := loadTestModel(t)
	vips, vipsheader, err := media.FindVips(filepath.Join("..", "vips", "bin", "vips"))
	if err != nil {
		t.Skip("no libvips")
	}
	library, shas := importPhotos(t, 3)
	s := NewService(m, library, media.NewPreviews(vips, vipsheader))
	s.scanNew(context.Background(), "hen", map[string]bool{})

	store := library.Store("hen")
	for _, sha := range shas {
		item, _ := store.Get(sha)
		d := item.Sidecar.Detected
		if d == nil || d.Nudity == nil || d.Nudity.Model != ModelName || d.Nudity.Score > 0.2 || d.Nudity.ScannedAt.IsZero() {
			t.Fatalf("%s: %+v", sha, d)
		}
	}
	if todo := ToScan(store, false, nil); len(todo) != 0 {
		t.Fatalf("still to scan: %d", len(todo))
	}
	if todo := ToScan(store, true, nil); len(todo) != 3 {
		t.Fatalf("to scan again: %d", len(todo))
	}
}
