package search

import (
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/journal/server-go/onnx"
)

var (
	testModelOnce sync.Once
	testModel     *Model
	testModelErr  error
)

// loadTestModel is the model scripts/bundle-onnx downloads, from the server folder; tests that need it skip
// without it.
func loadTestModel(t testing.TB) *Model {
	t.Helper()
	folder := filepath.Join("..", "models", "siglip2-base-patch16-256")
	library := filepath.Join("..", "onnxruntime", onnx.RuntimeLibrary())
	for _, need := range []string{folder, library} {
		if _, err := os.Stat(need); err != nil {
			t.Skipf("%s is missing (go run ./scripts/bundle-onnx)", need)
		}
	}
	testModelOnce.Do(func() {
		var rt *onnx.Runtime
		if rt, testModelErr = onnx.LoadRuntime(library); testModelErr == nil {
			testModel, testModelErr = LoadModel(rt, folder, 4)
		}
	})
	if testModelErr != nil {
		t.Fatal(testModelErr)
	}
	return testModel
}

func plain(c color.Color) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 320, 240))
	for y := range 240 {
		for x := range 320 {
			img.Set(x, y, c)
		}
	}
	return img
}

func TestModelMatchesTextToPictures(t *testing.T) {
	m := loadTestModel(t)
	red, err := m.ImageVector(plain(color.RGBA{220, 20, 20, 255}))
	if err != nil {
		t.Fatal(err)
	}
	blue, err := m.ImageVector(plain(color.RGBA{20, 40, 220, 255}))
	if err != nil {
		t.Fatal(err)
	}
	redText, err := m.TextVector("A plain red picture")
	if err != nil {
		t.Fatal(err)
	}
	blueText, err := m.TextVector("a plain blue picture")
	if err != nil {
		t.Fatal(err)
	}
	if len(red) != 768 || len(redText) != 768 {
		t.Fatalf("vectors of %d and %d numbers, want 768", len(red), len(redText))
	}
	t.Logf("red text: red %.3f blue %.3f; blue text: red %.3f blue %.3f",
		dot(redText, red), dot(redText, blue), dot(blueText, red), dot(blueText, blue))
	if dot(redText, red) <= dot(redText, blue) || dot(blueText, blue) <= dot(blueText, red) {
		t.Error("the colors' descriptions don't match their pictures best")
	}
}

// go test ./search -run - -bench . : how long reading a photo and a search's text take on this machine.
func BenchmarkImageVector(b *testing.B) {
	m := loadTestModel(b)
	img := plain(color.RGBA{120, 160, 60, 255})
	for b.Loop() {
		if _, err := m.ImageVector(img); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTextVector(b *testing.B) {
	m := loadTestModel(b)
	for b.Loop() {
		if _, err := m.TextVector("a dog on the beach at sunset"); err != nil {
			b.Fatal(err)
		}
	}
}

func TestTokensAreLowercasedAndPadded(t *testing.T) {
	m := loadTestModel(t)
	tokens := m.Tokens("A Dog")
	if len(tokens) != textLength {
		t.Fatalf("%d tokens, want %d", len(tokens), textLength)
	}
	if lower := m.Tokens("a dog"); lower[0] != tokens[0] || lower[1] != tokens[1] {
		t.Errorf("%v and %v differ: not lowercased", tokens[:3], lower[:3])
	}
	// "a dog" is two pieces, then <eos> (1) and <pad> (0) in Gemma's vocabulary.
	if tokens[2] != 1 || tokens[3] != 0 || tokens[textLength-1] != 0 {
		t.Errorf("tokens %v: want two words, <eos>, then <pad>", tokens[:5])
	}
	long := m.Tokens(strings.Repeat("word ", 100))
	if len(long) != textLength || long[textLength-1] != 1 {
		t.Errorf("a long text is %d tokens ending in %d; want %d ending in <eos>", len(long), long[len(long)-1], textLength)
	}
}

// The 8-bit text half phones may run (PhoneTextModelQ8) makes vectors close to the full one's, so its searches rank
// the photos' vectors (read with the full vision half) about the same.
func TestPhoneTextModel(t *testing.T) {
	full := loadTestModel(t)
	folder := filepath.Join("..", "models", "siglip2-base-patch16-256")
	if _, err := os.Stat(filepath.Join(folder, PhoneTextModelQ8)); err != nil {
		t.Skipf("%s is missing (go run ./scripts/bundle-onnx)", PhoneTextModelQ8)
	}
	rt, err := onnx.LoadRuntime(filepath.Join("..", "onnxruntime", onnx.RuntimeLibrary()))
	if err != nil {
		t.Fatal(err)
	}
	phone, err := LoadPhoneTextModel(rt, folder, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer phone.Close()
	colors := map[string]color.RGBA{"red": {220, 20, 20, 255}, "blue": {20, 40, 220, 255}, "green": {30, 180, 40, 255},
		"yellow": {230, 220, 30, 255}, "black": {10, 10, 10, 255}}
	pictures := map[string][]float32{}
	for name, c := range colors {
		if pictures[name], err = full.ImageVector(plain(c)); err != nil {
			t.Fatal(err)
		}
	}
	for _, text := range []string{"a plain red picture", "a dog on the beach at sunset", "קבלה מהסופר", "blue sky"} {
		a, err := full.TextVector(text)
		if err != nil {
			t.Fatal(err)
		}
		b, err := phone.TextVector(text)
		if err != nil {
			t.Fatal(err)
		}
		if cos := dot(a, b); cos < 0.98 {
			t.Errorf("%q: the 8-bit vector is %.3f like the full one's, want 0.98 or more", text, cos)
		}
	}
	for name := range colors {
		text := "a plain " + name + " picture"
		a, _ := full.TextVector(text)
		b, _ := phone.TextVector(text)
		best := func(q []float32) (string, float32) {
			var top string
			var score float32 = -2
			for n, v := range pictures {
				if s := dot(q, v); s > score {
					top, score = n, s
				}
			}
			return top, score
		}
		if fullBest, _ := best(a); fullBest != name {
			continue // the full model doesn't find it either
		}
		if phoneBest, _ := best(b); phoneBest != name {
			t.Errorf("%q: the 8-bit text half finds %s first, the full one %s", text, phoneBest, name)
		}
	}
}
