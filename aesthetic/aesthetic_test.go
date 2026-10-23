package aesthetic

import (
	"image"
	"image/color"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/journal/server-go/onnx"
)

// Scores are on LAION's scale, and random noise scores below a plain grey picture (it is no photo at all).
func TestScoresAreOnLAIONsScale(t *testing.T) {
	folder := filepath.Join("..", "models", "clip-vit-large-patch14-laion")
	library := filepath.Join("..", "onnxruntime", onnx.RuntimeLibrary())
	for _, need := range []string{filepath.Join(folder, "vision_model.onnx"), filepath.Join(folder, "laion-head.json"), library} {
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
	grey, noise := image.NewRGBA(image.Rect(0, 0, m.size, m.size)), image.NewRGBA(image.Rect(0, 0, m.size, m.size))
	rng := rand.New(rand.NewPCG(1, 2))
	for y := range m.size {
		for x := range m.size {
			grey.Set(x, y, color.Gray{128})
			noise.Set(x, y, color.RGBA{uint8(rng.IntN(256)), uint8(rng.IntN(256)), uint8(rng.IntN(256)), 255})
		}
	}
	g, err := m.Score(grey)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := m.Score(noise)
	if g < 1 || g > 10 || n < 1 || n > 10 || n >= g {
		t.Errorf("grey %.2f, noise %.2f", g, n)
	}
	if _, err := m.Score(image.NewRGBA(image.Rect(0, 0, 10, 10))); err == nil {
		t.Error("scored a picture of the wrong size")
	}
}
