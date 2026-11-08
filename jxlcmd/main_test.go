package jxlcmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/jpeg"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/media"
)

func testJXL(t *testing.T) *media.JXL {
	t.Helper()
	vips, _, err := media.FindVips(filepath.Join("..", "..", "vips", "bin", "vips"))
	if err != nil {
		vips, _, _ = media.FindVips("")
	}
	x, err := media.LoadJXL(media.JXLLibraries(vips, ""))
	if err != nil {
		t.Skipf("libjxl not installed: %v", err)
	}
	return x
}

func photo(t *testing.T, seed int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 160, 120))
	for y := range 120 {
		for x := range 160 {
			img.Set(x, y, color.RGBA{uint8(x + seed), uint8(y * seed), uint8(x ^ y), 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func place(t *testing.T, store *media.Store, data []byte, name, mime string) media.Item {
	t.Helper()
	sum := sha256.Sum256(data)
	taken := time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC)
	req := media.UploadRequest{Sha256: hex.EncodeToString(sum[:]), Size: int64(len(data)), FileName: name, MimeType: mime,
		Kind: media.KindPhoto, TakenAt: &taken, UploadedBy: "test"}
	if _, err := store.StartUpload(req); err != nil {
		t.Fatal(err)
	}
	r, err := store.WriteChunk(req.Sha256, 0, req.Size, bytes.NewReader(data), req.Size)
	if err != nil || r.Item == nil {
		t.Fatalf("placing %s: %+v %v", name, r, err)
	}
	return *r.Item
}

func TestConvertKeepsJPEGsAndReportsTheSaving(t *testing.T) {
	x := testJXL(t)
	store, err := media.OpenStore(t.TempDir(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	a := place(t, store, photo(t, 1), "a.jpg", "image/jpeg")
	place(t, store, photo(t, 2), "b.JPG", "image/jpeg")
	place(t, store, []byte("a HEIC photo"), "c.heic", "image/heic")
	place(t, store, []byte("not really a jpeg"), "d.jpg", "image/jpeg")

	todo := jpegs(store, nil, false)
	if len(todo) != 3 {
		t.Fatalf("%d JPEGs to keep", len(todo))
	}
	var out bytes.Buffer
	if err := estimate(context.Background(), store, x, todo, 10, 2, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "sample: 2 kept") || !strings.Contains(out.String(), "1 would stay JPEG") {
		t.Errorf("estimate:\n%s", out.String())
	}
	if _, ok := store.Get(a.Sha256()); !ok {
		t.Fatal("the estimate changed the store")
	}

	store.JXL = x
	run, err := convert(context.Background(), store, todo, 2, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if run.kept != 2 || run.stayed != 1 || len(run.failed) != 1 || run.jxlBytes >= run.jpegBytes {
		t.Fatalf("run %+v", run)
	}
	if err := writeFailed(store, run.failed); err != nil {
		t.Fatal(err)
	}
	if left := jpegs(store, readFailed(store), false); len(left) != 0 {
		t.Errorf("left to keep after the run: %d", len(left))
	}
	if left := jpegs(store, readFailed(store), true); len(left) != 1 {
		t.Errorf("left to retry: %d", len(left))
	}
	s := savings(store)
	if s.kept != 2 || s.jpegs != 1 || s.keptJPEG != run.jpegBytes || s.keptJXL != run.jxlBytes {
		t.Errorf("savings %+v, run %+v", s, run)
	}
	out.Reset()
	s.print(&out)
	if !strings.Contains(out.String(), "JPEGs kept as JPEG XL: 2") || !strings.Contains(out.String(), "%") {
		t.Errorf("report:\n%s", out.String())
	}
	t.Log("\n" + out.String())
}

func TestNextTime(t *testing.T) {
	now := time.Date(2026, 10, 28, 23, 30, 0, 0, time.Local)
	if at, _ := nextTime("07:00", now); !at.Equal(time.Date(2026, 10, 29, 7, 0, 0, 0, time.Local)) {
		t.Errorf("07:00 after 23:30 = %v", at)
	}
	if at, _ := nextTime("23:45", now); !at.Equal(time.Date(2026, 10, 28, 23, 45, 0, 0, time.Local)) {
		t.Errorf("23:45 after 23:30 = %v", at)
	}
	if _, err := nextTime("7am", now); err == nil {
		t.Error("took 7am")
	}
}
