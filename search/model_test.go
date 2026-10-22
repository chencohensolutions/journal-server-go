package search

import (
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
	library := filepath.Join("..", "onnxruntime", runtimeLibrary)
	for _, need := range []string{folder, library} {
		if _, err := os.Stat(need); err != nil {
			t.Skipf("%s is missing (go run ./scripts/bundle-onnx)", need)
		}
	}
	testModelOnce.Do(func() {
		var rt *Runtime
		if rt, testModelErr = LoadRuntime(library); testModelErr == nil {
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

func dot(a, b []float32) float32 {
	var sum float32
	for i := range a {
		sum += a[i] * b[i]
	}
	return sum
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
