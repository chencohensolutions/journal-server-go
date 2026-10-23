package similar

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/image/draw"

	"github.com/journal/server-go/onnx"
)

func loadTestModel(t *testing.T) *Model {
	t.Helper()
	folder := filepath.Join("..", "models", "dinov2-small")
	library := filepath.Join("..", "onnxruntime", onnx.RuntimeLibrary())
	for _, need := range []string{filepath.Join(folder, "model.onnx"), library} {
		if _, err := os.Stat(need); err != nil {
			t.Skipf("%s is missing (go run ./scripts/bundle-onnx)", need)
		}
	}
	rt, err := onnx.LoadRuntime(library)
	if err != nil {
		t.Fatal(err)
	}
	m, err := LoadModel(rt, folder, 4)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// scene is a made-up picture of coloured blocks, different for each seed.
func scene(seed uint64, w, h int) *image.RGBA {
	rng := rand.New(rand.NewPCG(seed, seed))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for range 40 {
		c := color.RGBA{uint8(rng.IntN(256)), uint8(rng.IntN(256)), uint8(rng.IntN(256)), 255}
		x, y := rng.IntN(w), rng.IntN(h)
		draw.Draw(img, image.Rect(x, y, x+rng.IntN(w/3)+10, y+rng.IntN(h/3)+10), image.NewUniform(c), image.Point{}, draw.Src)
	}
	return img
}

func squareOf(m *Model, img image.Image) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, m.size, m.size))
	draw.CatmullRom.Scale(out, out.Bounds(), img, img.Bounds(), draw.Src, nil)
	return out
}

// A smaller, recompressed copy of a picture is a duplicate of it; another picture isn't.
func TestACopyIsADuplicateAndAnotherPictureIsNot(t *testing.T) {
	m := loadTestModel(t)
	a := scene(1, 600, 600)
	small := image.NewRGBA(image.Rect(0, 0, 400, 400))
	draw.CatmullRom.Scale(small, small.Bounds(), a, a.Bounds(), draw.Src, nil)
	var buf bytes.Buffer
	jpeg.Encode(&buf, small, &jpeg.Options{Quality: 60})
	copied, err := jpeg.Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	va, _ := m.Vector(squareOf(m, a))
	vc, _ := m.Vector(squareOf(m, copied))
	// Another kind of picture: stripes (two made-up block pictures look alike to DINOv2, as real photos don't).
	stripes := image.NewRGBA(image.Rect(0, 0, 600, 600))
	for y := range 600 {
		for x := range 600 {
			stripes.Set(x, y, color.RGBA{uint8(120 + 120*((y/30)%2)), uint8(x * 255 / 600), 60, 255})
		}
	}
	vb, _ := m.Vector(squareOf(m, stripes))
	if len(va) != m.Dim || m.Dim != 384 {
		t.Fatalf("vectors of %d numbers", len(va))
	}
	if s := dot(va, vc); s < DuplicateSimilarity {
		t.Errorf("a copy scores %.3f, under %.2f", s, DuplicateSimilarity)
	}
	if s := dot(va, vb); s >= DuplicateSimilarity {
		t.Errorf("another picture scores %.3f", s)
	}
}

// Copies that link through each other are one group; the rest are none.
func TestDuplicatesAreGroupedAcrossLinks(t *testing.T) {
	u := newUnionFind()
	u.join("a", "b")
	u.join("c", "b")
	u.join("x", "y")
	sets := u.sets()
	for _, s := range sets {
		slices.Sort(s)
	}
	slices.SortFunc(sets, func(a, b []string) int { return len(b) - len(a) })
	if len(sets) != 2 || !slices.Equal(sets[0], []string{"a", "b", "c"}) || !slices.Equal(sets[1], []string{"x", "y"}) {
		t.Errorf("sets: %v", sets)
	}
}
