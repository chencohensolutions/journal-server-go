package copies

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/journal/server-go/media"
)

func testJXL(t *testing.T) *media.JXL {
	t.Helper()
	vips, _, err := media.FindVips(filepath.Join("..", "vips", "bin", "vips"))
	if err != nil {
		vips, _, _ = media.FindVips("")
	}
	x, err := media.LoadJXL(media.JXLLibraries(vips, ""))
	if err != nil {
		t.Skipf("libjxl not installed: %v", err)
	}
	return x
}

func photoJPEG(t *testing.T, seed uint8) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 160, 120))
	for y := range 120 {
		for x := range 160 {
			img.Set(x, y, color.RGBA{uint8(x) + seed, uint8(y) * seed, uint8(x ^ y), 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// A JPEG the data folder keeps as JPEG XL leaves the copy once the copy has the JPEG XL and it makes the copy's JPEG
// byte for byte; without libjxl, or when the copy's JPEG isn't that JPEG, it stays.
func TestAJPEGKeptAsJXLLeavesTheCopy(t *testing.T) {
	x := testJXL(t)
	store, c := newCopier(t)
	a := addPhoto(t, store, "IMG_1.jpg", photoJPEG(t, 1))
	b := addPhoto(t, store, "IMG_2.jpg", photoJPEG(t, 2))
	if pass := c.Run(context.Background()); pass.Err != nil || pass.Failed != 0 {
		t.Fatalf("first pass: %+v", pass)
	}
	month := filepath.Join(c.Folder, "media", "2026", "2026-10")
	// The copy's IMG_2.jpg isn't the data folder's JPEG any more (say, damaged on the copy's disk).
	os.WriteFile(filepath.Join(month, "IMG_2.jpg"), []byte(photoJPEG(t, 3)), 0o644)

	store.JXL = x
	for _, sha := range []string{a, b} {
		if _, err := store.KeepAsJXL(sha); err != nil {
			t.Fatal(err)
		}
	}
	store.JXL = nil // without libjxl nothing is checked, so nothing leaves
	if pass := c.Run(context.Background()); pass.Dropped != 0 || pass.Failed != 2 {
		t.Fatalf("pass without libjxl: %+v", pass)
	}
	if !exists(filepath.Join(month, "IMG_1.jxl")) || !exists(filepath.Join(month, "IMG_1.jpg")) {
		t.Fatal("the JPEG XL wasn't copied, or the JPEG left without a check")
	}

	store.JXL = x
	pass := c.Run(context.Background())
	if pass.Dropped != 1 || pass.DroppedBytes != int64(len(photoJPEG(t, 1))) || pass.Failed != 1 {
		t.Fatalf("pass: %+v", pass)
	}
	if exists(filepath.Join(month, "IMG_1.jpg")) || exists(filepath.Join(month, "IMG_1.jpg.json")) {
		t.Error("the copy still has IMG_1.jpg or its sidecar")
	}
	if !exists(filepath.Join(month, "IMG_1.jxl")) || !exists(filepath.Join(month, "IMG_1.jxl.json")) {
		t.Error("the copy has no IMG_1.jxl or its sidecar")
	}
	if !exists(filepath.Join(month, "IMG_2.jpg")) {
		t.Error("a JPEG that isn't the one kept as JPEG XL left the copy")
	}
	if again := c.Run(context.Background()); again.Dropped != 0 || again.Files != 0 {
		t.Errorf("a pass after: %+v", again)
	}
}
