package faces

import (
	"image"
	_ "image/jpeg"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/image/draw"

	"github.com/journal/server-go/onnx"
)

var (
	testModelOnce sync.Once
	testModel     *Model
	testModelErr  error
)

// loadTestModel is the models scripts/bundle-onnx downloads, from the server folder; tests that need them skip
// without them.
func loadTestModel(t testing.TB) *Model {
	t.Helper()
	folder := filepath.Join("..", "models", "yunet-sface")
	library := filepath.Join("..", "onnxruntime", onnx.RuntimeLibrary())
	for _, need := range []string{filepath.Join(folder, detectorFile), filepath.Join(folder, recognizerFile), library} {
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

// testPhotos are the JPEGs in $JOURNAL_FACE_TESTDATA, photos with faces in them: none are kept in the repository.
// Tests that need them skip without.
func testPhotos(t *testing.T) []string {
	t.Helper()
	dir := os.Getenv("JOURNAL_FACE_TESTDATA")
	if dir == "" {
		t.Skip("JOURNAL_FACE_TESTDATA is not set (a folder of JPEG photos with faces)")
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*.jpg"))
	if len(paths) == 0 {
		t.Skipf("no .jpg in %s", dir)
	}
	return paths
}

func readPhoto(t *testing.T, path string) image.Image {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// scaled is img with its longer side n pixels, as a preview is.
func scaled(img image.Image, n int) image.Image {
	b := img.Bounds()
	w, h := n, b.Dy()*n/b.Dx()
	if b.Dy() > b.Dx() {
		w, h = b.Dx()*n/b.Dy(), n
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(out, out.Bounds(), img, b, draw.Src, nil)
	return out
}

// A face is found in each photo, and found again, as the same person, in the photo at another size: the box lands
// in the same place (it is a fraction of the picture) and the vectors agree well above the 0.363 OpenCV takes for
// the same person.
func TestFacesAreFoundAgainAtAnotherSize(t *testing.T) {
	m := loadTestModel(t)
	for _, path := range testPhotos(t) {
		img := readPhoto(t, path)
		big, err := m.Faces(scaled(img, 1280))
		if err != nil {
			t.Fatal(err)
		}
		small, err := m.Faces(scaled(img, 900))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %d faces at 1280, %d at 900", filepath.Base(path), len(big), len(small))
		if len(big) == 0 {
			t.Errorf("%s: no face found", path)
			continue
		}
		for _, f := range big {
			if f.Box[2]*1280 < 64 { // small faces change more with the size
				continue
			}
			best, bestIoU := Face{}, float32(0)
			for _, g := range small {
				if o := iou(f.Box, g.Box); o > bestIoU {
					best, bestIoU = g, o
				}
			}
			if bestIoU < 0.5 {
				t.Errorf("%s: the face at %v is not found at 900 px", filepath.Base(path), f.Box)
				continue
			}
			if sim := dot(f.Vector, best.Vector); sim < 0.5 {
				t.Errorf("%s: the face at %v is only %.2f like itself at 900 px", filepath.Base(path), f.Box, sim)
			}
		}
	}
}
