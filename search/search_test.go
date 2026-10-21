package search

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/journal/server-go/media"
	"github.com/journal/server-go/search/searchtest"
)

func TestMain(m *testing.M) {
	if os.Getenv(searchtest.Env) == "1" {
		searchtest.Serve()
		os.Exit(0)
	}
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// picture is a small PNG of one colour.
func picture(c color.RGBA) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, 255
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

// importFile puts data in store as if a device had uploaded it, and returns its sha256.
func importFile(t *testing.T, store *media.Store, data []byte, name string) string {
	t.Helper()
	source := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(source, data, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	_, _, err := store.Import(source, media.UploadRequest{
		Sha256: sha, Size: int64(len(data)), FileName: name, MimeType: "image/png", Kind: media.KindPhoto, UploadedBy: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

// originals gives the model the originals themselves, so the tests need no libvips.
func originals(_ context.Context, store *media.Store, item media.Item) (string, func(), error) {
	return filepath.Join(store.Root(), filepath.FromSlash(item.Path)), nil, nil
}

// startEngine runs an engine with the stand-in model over hen's store until the test ends.
func startEngine(t *testing.T, store *media.Store) *Engine {
	t.Helper()
	t.Setenv(searchtest.Env, "1")
	library, err := media.OpenLibrary(map[string]string{"hen": store.Root()}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	engine := New(library, "test/colours", []string{os.Args[0]}, originals)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { engine.Run(ctx); close(done) }()
	t.Cleanup(func() { stop(); <-done })
	return engine
}

// searchWhenRead searches once indexed files have been read, or fails after a few seconds.
func searchWhenRead(t *testing.T, engine *Engine, query string, indexed int, hide func(media.Item) bool) Result {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		result, err := engine.Search("hen", query, 10, hide)
		if err == nil && result.Indexed >= indexed {
			return result
		}
		if err != nil && !errors.Is(err, ErrNotReady) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("search %q: %+v, %v", query, result, err)
		}
	}
}

func TestSearchFindsFilesByWhatTheyShow(t *testing.T) {
	store, err := media.OpenStore(t.TempDir(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	red := importFile(t, store, picture(color.RGBA{R: 250, G: 10, B: 10}), "red.png")
	blue := importFile(t, store, picture(color.RGBA{R: 10, G: 20, B: 240}), "blue.png")
	importFile(t, store, []byte("not a picture"), "broken.png")
	// The model fails a batch with this one as a whole; the others in it are read one by one.
	importFile(t, store, picture(color.RGBA{R: 128, G: 128, B: 128}), "choke.png")
	engine := startEngine(t, store)

	result := searchWhenRead(t, engine, "  red ", 2, nil)
	if result.Total != 4 || result.Indexed != 2 || len(result.Hits) != 2 {
		t.Fatalf("result = %+v", result)
	}
	if result.Hits[0].Item.Sha256() != red || result.Hits[1].Item.Sha256() != blue || result.Hits[0].Score < 0.99 {
		t.Fatalf("red first: %+v", result.Hits)
	}
	if result := searchWhenRead(t, engine, "blue", 2, nil); result.Hits[0].Item.Sha256() != blue {
		t.Fatalf("blue first: %+v", result.Hits)
	}

	// Hidden files are neither found nor counted.
	result = searchWhenRead(t, engine, "red", 1, func(item media.Item) bool { return item.Sha256() == red })
	if result.Total != 3 || result.Indexed != 1 || len(result.Hits) != 1 || result.Hits[0].Item.Sha256() != blue {
		t.Fatalf("with red hidden: %+v", result)
	}

	// A file added later is read on the next look; Wake makes that now.
	green := importFile(t, store, picture(color.RGBA{G: 200}), "green.png")
	engine.Wake()
	if result := searchWhenRead(t, engine, "green", 3, nil); result.Hits[0].Item.Sha256() != green {
		t.Fatalf("green first: %+v", result.Hits)
	}

	if _, err := engine.Search("hen", " ", 10, nil); err == nil {
		t.Fatal("an empty query was searched")
	}
}

func TestVectorsAreKeptAcrossRestarts(t *testing.T) {
	root := t.TempDir()
	v, err := openVectors(root, "test/model", 3)
	if err != nil {
		t.Fatal(err)
	}
	a, b := hex.EncodeToString(bytes.Repeat([]byte{0xaa}, 32)), hex.EncodeToString(bytes.Repeat([]byte{0xbb}, 32))
	if err := v.add([]string{a, b}, [][]float32{{0.6, 0.8, 0}, {0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	v.close()

	// A record cut short by a crash is dropped; the others stay.
	path := vectorsPath(root, "test/model")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.Write([]byte{1, 2, 3})
	f.Close()
	v, err = openVectors(root, "test/model", 3)
	if err != nil {
		t.Fatal(err)
	}
	scores := v.scores([]float32{0.6, 0.8, 0}, []string{a, b, "cc"})
	if scores[0] < 0.99 || scores[0] > 1.01 || scores[1] != 0 || scores[2] == scores[2] {
		t.Fatalf("scores = %v", scores)
	}
	c := hex.EncodeToString(bytes.Repeat([]byte{0xcc}, 32))
	if err := v.add([]string{c}, [][]float32{{1, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	v.close()
	if v, _ = openVectors(root, "test/model", 3); !v.has(a) || !v.has(c) || len(v.scales) != 3 {
		t.Fatalf("after adding to a repaired file: %d vectors", len(v.scales))
	}
	v.close()

	// Another vector size (a new model under the same name) starts again.
	v, err = openVectors(root, "test/model", 4)
	if err != nil {
		t.Fatal(err)
	}
	defer v.close()
	if v.has(a) {
		t.Fatal("vectors of another size were kept")
	}
}
