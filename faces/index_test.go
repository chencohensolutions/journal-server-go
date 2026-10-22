package faces

import (
	"math"
	"os"
	"strings"
	"testing"
)

func testFace(seed float32) Face {
	f := Face{Box: [4]float32{0.1, 0.2, 0.3, 0.4}, Score: 0.9, Vector: make([]float32, Dim)}
	for i := range f.Landmarks {
		f.Landmarks[i] = seed + float32(i)/100
	}
	f.Vector[int(seed)%Dim] = 1
	return f
}

func TestIndexKeepsFacesAcrossRestarts(t *testing.T) {
	root := t.TempDir()
	x, err := OpenIndex(root, "test")
	if err != nil {
		t.Fatal(err)
	}
	a, b, none := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	if err := x.Add(a, []Face{testFace(1), testFace(2)}); err != nil {
		t.Fatal(err)
	}
	if err := x.Add(b, []Face{testFace(3)}); err != nil {
		t.Fatal(err)
	}
	if err := x.Add(none, nil); err != nil {
		t.Fatal(err)
	}
	// A file read again (another model's faces) has its faces replaced.
	if err := x.Add(b, []Face{testFace(4)}); err != nil {
		t.Fatal(err)
	}
	if err := x.Add("nope", nil); err == nil {
		t.Error("added a file that isn't a sha256")
	}
	if err := x.Add(a, []Face{{Vector: []float32{1}}}); err == nil {
		t.Error("added a vector of the wrong length")
	}
	x.Close()

	// The server stopped while writing a record: it is dropped.
	f, err := os.OpenFile(x.path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("cut short"))
	f.Close()

	x, err = OpenIndex(root, "test")
	if err != nil {
		t.Fatal(err)
	}
	if files, faces := x.Len(); files != 3 || faces != 3 {
		t.Errorf("%d files and %d faces, want 3 and 3", files, faces)
	}
	if !x.Has(none) || len(x.Faces(none)) != 0 {
		t.Error("a file without faces is read, with none")
	}
	got := x.Faces(a)
	if len(got) != 2 || got[1].Landmarks != testFace(2).Landmarks || got[1].Box != testFace(2).Box || got[1].Vector[2] != 1 {
		t.Errorf("faces of a: %+v", got)
	}
	if got := x.Faces(b); len(got) != 1 || got[0].Vector[4] != 1 {
		t.Errorf("b's faces were not replaced: %+v", got)
	}
	// Records added after the cut one are kept.
	if err := x.Add(none, []Face{testFace(5)}); err != nil {
		t.Fatal(err)
	}
	x.Close()
	if x, err = OpenIndex(root, "test"); err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	if got := x.Faces(none); len(got) != 1 {
		t.Errorf("after reopening: %+v", got)
	}
}

func TestAlignFitsTheTemplate(t *testing.T) {
	// The template turned by 30°, scaled by 2 and moved: fit finds the way back.
	angle := math.Pi / 6
	turn := similarity{a: float32(2 * math.Cos(angle)), b: float32(2 * math.Sin(angle)), tx: 300, ty: -40}
	var points [10]float32
	for i := range 5 {
		points[2*i], points[2*i+1] = turn.apply(template[2*i], template[2*i+1])
	}
	back := fit(points, template)
	for i := range 5 {
		x, y := back.apply(points[2*i], points[2*i+1])
		if math.Abs(float64(x-template[2*i])) > 1e-3 || math.Abs(float64(y-template[2*i+1])) > 1e-3 {
			t.Errorf("point %d lands at %.3f,%.3f, not %.3f,%.3f", i, x, y, template[2*i], template[2*i+1])
		}
	}
	x, y := back.inverse().apply(template[0], template[1])
	if math.Abs(float64(x-points[0])) > 1e-2 || math.Abs(float64(y-points[1])) > 1e-2 {
		t.Errorf("inverse: %.3f,%.3f, want %.3f,%.3f", x, y, points[0], points[1])
	}
}

func TestDetectionsThatOverlapAreOne(t *testing.T) {
	kept := suppress([]detection{
		{box: [4]float32{0, 0, 10, 10}, score: 0.7},
		{box: [4]float32{1, 1, 10, 10}, score: 0.9},
		{box: [4]float32{50, 50, 10, 10}, score: 0.8},
	})
	if len(kept) != 2 || kept[0].score != 0.9 || kept[1].score != 0.8 {
		t.Errorf("kept %+v", kept)
	}
	if got := tileStarts(1280); len(got) != 3 || got[1] != 320 || got[2] != 640 {
		t.Errorf("tiles over 1280: %v", got)
	}
	if got := tileStarts(700); len(got) != 3 || got[2] != 60 {
		t.Errorf("tiles over 700: %v", got)
	}
	if got := tileStarts(640); len(got) != 1 {
		t.Errorf("tiles over 640: %v", got)
	}
}
