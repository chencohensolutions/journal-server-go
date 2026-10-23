package nudity

import (
	"image"
	"image/color"
	_ "image/jpeg"
	"math"
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
	path := filepath.Join("..", "models", "nudenet", File)
	library := filepath.Join("..", "onnxruntime", onnx.RuntimeLibrary())
	for _, need := range []string{path, library} {
		if _, err := os.Stat(need); err != nil {
			t.Skipf("%s is missing (go run ./scripts/bundle-onnx)", need)
		}
	}
	testModelOnce.Do(func() {
		var rt *onnx.Runtime
		if rt, testModelErr = onnx.LoadRuntime(library); testModelErr == nil {
			testModel, testModelErr = LoadModel(rt, path, 4)
		}
	})
	if testModelErr != nil {
		t.Fatal(testModelErr)
	}
	return testModel
}

// The picture is padded with black at the right (or bottom) and shrunk from pixel centres; blue comes first.
func TestInputPadsAndShrinksAsOpenCV(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 640, 320)) // twice as wide as high: the bottom half is padding
	for y := range 320 {
		for x := range 640 {
			img.Set(x, y, color.RGBA{R: 200, G: 100, B: uint8(x / 4), A: 255})
		}
	}
	pixels := input(img)
	const plane = side * side
	at := func(p, x, y int) float32 { return pixels[p*plane+y*side+x] * 255 }
	// 640 → 320 is half: each output pixel is the mean of two inputs across (4x and 4x+1 have the same blue).
	if r, g := at(2, 10, 10), at(1, 10, 10); r != 200 || g != 100 {
		t.Fatalf("red %v, green %v", r, g)
	}
	if b := at(0, 101, 10); math.Abs(float64(b)-50) > 0.5 {
		t.Fatalf("blue at 101: %v, want 50", b)
	}
	if r := at(2, 10, 200); r != 0 {
		t.Fatalf("padding: %v", r)
	}
}

// A cell becomes a box of its surest label; overlapping boxes are one, whatever their labels; only exposed parts
// count.
func TestDetectionsAreReadAsNudeNet(t *testing.T) {
	out := make([]float32, (4+len(labels))*cells)
	set := func(cell int, box [4]float32, label int, score float32) {
		for i, v := range box {
			out[i*cells+cell] = v
		}
		out[(4+label)*cells+cell] = score
	}
	index := func(name string) int {
		for i, l := range labels {
			if l == name {
				return i
			}
		}
		t.Fatal(name)
		return 0
	}
	set(0, [4]float32{100, 100, 50, 50}, index("FEMALE_BREAST_EXPOSED"), 0.7)
	set(1, [4]float32{102, 101, 50, 50}, index("BELLY_EXPOSED"), 0.9)          // the same place, surer: hides the first
	set(2, [4]float32{250, 250, 40, 40}, index("BUTTOCKS_EXPOSED"), 0.4)       // elsewhere
	set(3, [4]float32{50, 250, 40, 40}, index("MALE_GENITALIA_EXPOSED"), 0.22) // over minScore, not over keepScore
	set(4, [4]float32{20, 20, 10, 10}, index("ANUS_EXPOSED"), 0.1)             // under minScore
	r := answer(detections(out, 640, 640))
	if math.Abs(r.Score-0.4) > 1e-6 || strings.Join(r.Found, ",") != "BUTTOCKS_EXPOSED" {
		t.Fatalf("%+v", r)
	}
	if r := answer(nil); r.Score != 0 || r.Found != nil {
		t.Fatalf("nothing found: %+v", r)
	}
}

func TestTheModelRuns(t *testing.T) {
	m := loadTestModel(t)
	img := image.NewRGBA(image.Rect(0, 0, 480, 640))
	for y := range 640 {
		for x := range 480 {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	r, err := m.Score(img)
	if err != nil {
		t.Fatal(err)
	}
	if r.Score > 0.2 {
		t.Fatalf("a gradient scored %+v", r)
	}
}

// testPhotos are the JPEGs in $JOURNAL_NUDITY_TESTDATA: none are kept in the repository. Each is scored and
// printed (go test -v), to compare with NudeNet; tests that need them skip without.
func TestScorePhotos(t *testing.T) {
	dir := os.Getenv("JOURNAL_NUDITY_TESTDATA")
	if dir == "" {
		t.Skip("set JOURNAL_NUDITY_TESTDATA to a folder of JPEGs")
	}
	m := loadTestModel(t)
	paths, _ := filepath.Glob(filepath.Join(dir, "*.jpg"))
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		img, _, err := image.Decode(f)
		f.Close()
		if err != nil {
			t.Fatal(path, err)
		}
		all, err := m.found(img)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, d := range all {
			names = append(names, labels[d.label])
		}
		t.Logf("%.3f %s %v", answer(all).Score, filepath.Base(path), names)
	}
}
