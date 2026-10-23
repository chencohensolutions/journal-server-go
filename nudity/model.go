// Package nudity scores photos and videos for nudity with NudeNet's detector (320n, a YOLOv8 model by notAI-tech,
// AGPL-3.0), run by ONNX Runtime inside the server, with no Python. It reads the same 640 px preview as search (a
// video's poster frame) and writes its answer into the file's sidecar ("detected": {"nudity": …}), which the
// server then uses to hide what the user chose to hide (media.Hidden). See docs/media.md, "Nudity scan".
package nudity

import (
	"fmt"
	"image"
	"image/draw"
	"math"
	"sort"

	"github.com/journal/server-go/onnx"
)

// ModelName names the detector in the sidecar. Change it when the way of scoring changes, so files scan again.
const ModelName = "nudenet-320n"

// File is the model's file, as scripts/bundle-onnx downloads it from NudeNet's releases (v3.4-weights).
const File = "320n.onnx"

// The model reads a 320×320 picture and answers, for each of its 2100 cells (every 8, 16 and 32 pixels), a box
// (centre, width and height, in its pixels) and how sure it is of each of the 18 labels.
const (
	side  = 320
	cells = 2100
)

// labels are the model's, in its order.
var labels = [...]string{
	"FEMALE_GENITALIA_COVERED", "FACE_FEMALE", "BUTTOCKS_EXPOSED", "FEMALE_BREAST_EXPOSED",
	"FEMALE_GENITALIA_EXPOSED", "MALE_BREAST_EXPOSED", "ANUS_EXPOSED", "FEET_EXPOSED", "BELLY_COVERED",
	"FEET_COVERED", "ARMPITS_COVERED", "ARMPITS_EXPOSED", "FACE_MALE", "BELLY_EXPOSED", "MALE_GENITALIA_EXPOSED",
	"ANUS_COVERED", "FEMALE_BREAST_COVERED", "BUTTOCKS_COVERED",
}

// exposed are the labels that count as nudity.
var exposed = map[string]bool{
	"FEMALE_BREAST_EXPOSED": true, "FEMALE_GENITALIA_EXPOSED": true, "MALE_GENITALIA_EXPOSED": true,
	"BUTTOCKS_EXPOSED": true, "ANUS_EXPOSED": true,
}

// As NudeNet's own detect: a cell counts when its surest label reaches minScore; of boxes that overlap more than
// overlapSame (whatever their labels), the surest is kept, of those over keepScore.
const (
	minScore    = 0.2
	keepScore   = 0.25
	overlapSame = 0.45
)

// Model is NudeNet's detector, loaded in ONNX Runtime.
type Model struct {
	session *onnx.Session
}

// LoadModel loads the model at path, run with up to threads CPU threads.
func LoadModel(rt *onnx.Runtime, path string, threads int) (*Model, error) {
	session, err := rt.NewSession(path, threads)
	if err != nil {
		return nil, err
	}
	if len(session.Inputs) != 1 || len(session.Outputs) != 1 {
		return nil, fmt.Errorf("%s takes %v and gives %v; expected one of each", path, session.Inputs, session.Outputs)
	}
	if err := session.Use(session.Inputs, session.Outputs); err != nil {
		return nil, err
	}
	return &Model{session: session}, nil
}

// Result is the model's answer about a picture.
type Result struct {
	// The detector's highest confidence in any exposed part, 0 to 1; 0 when it finds none.
	Score float64
	// The exposed parts found, by label, sorted.
	Found []string
}

// Score scores img: as NudeNet does, padded with black to a square at the right or bottom, shrunk to 320×320 and
// given to the model, whose boxes are then put together.
func (m *Model) Score(img image.Image) (Result, error) {
	found, err := m.found(img)
	if err != nil {
		return Result{}, err
	}
	return answer(found), nil
}

// found is everything the model finds in img, exposed or not, surest first.
func (m *Model) found(img image.Image) ([]detection, error) {
	out, err := onnx.Run(m.session, onnx.Tensor[float32]{Data: input(img), Shape: []int64{1, 3, side, side}})
	if err != nil {
		return nil, err
	}
	if len(out.Data) != (4+len(labels))*cells {
		return nil, fmt.Errorf("the model gave %v numbers, not %d×%d", out.Shape, 4+len(labels), cells)
	}
	b := img.Bounds()
	return detections(out.Data, b.Dx(), b.Dy()), nil
}

// input is img as the model reads it: padded with black to a square (right and bottom), shrunk to 320×320 the way
// OpenCV does (bilinear, from pixel centres, rounded to whole levels), as blue, green and red planes of 0 to 1.
// Blue first is how NudeNet gives it: OpenCV reads a picture as blue, green, red, NudeNet turns that to red, green,
// blue, and then asks for red and blue swapped again. The scores it was tested with come from that order.
func input(img image.Image) []float32 {
	rgba := toRGBA(img)
	w, h := rgba.Bounds().Dx(), rgba.Bounds().Dy()
	size := max(w, h)
	scale := float64(size) / side
	// Where each output column and row reads from: the two pixels either side, and how much of the second.
	type tap struct {
		a, b int
		f    float64
	}
	taps := make([]tap, side)
	for d := range taps {
		s := (float64(d)+0.5)*scale - 0.5
		if s < 0 {
			s = 0
		}
		a := int(math.Floor(s))
		f := s - float64(a)
		if a >= size-1 {
			a, f = size-1, 0
		}
		taps[d] = tap{a, min(a+1, size-1), f}
	}
	at := func(x, y, c int) float64 {
		if x >= w || y >= h {
			return 0 // the black padding
		}
		return float64(rgba.Pix[rgba.PixOffset(x, y)+c])
	}
	const plane = side * side
	pixels := make([]float32, 3*plane)
	for dy, ty := range taps {
		for dx, tx := range taps {
			i := dy*side + dx
			for p, c := range [3]int{2, 1, 0} { // blue, green, red
				top := at(tx.a, ty.a, c)*(1-tx.f) + at(tx.b, ty.a, c)*tx.f
				bottom := at(tx.a, ty.b, c)*(1-tx.f) + at(tx.b, ty.b, c)*tx.f
				level := math.Round(top*(1-ty.f) + bottom*ty.f)
				pixels[p*plane+i] = float32(level / 255)
			}
		}
	}
	return pixels
}

// detection is a box the model found, in pixels of the picture scored.
type detection struct {
	box   [4]float64 // x, y, w, h
	score float64
	label int
}

// detections reads the model's output (4 box numbers and 18 label scores, each for every cell) as NudeNet does:
// each cell whose surest label reaches minScore gives a box of that label, in the picture's pixels and cut to it;
// then of boxes that overlap, only the surest is kept.
func detections(out []float32, w, h int) []detection {
	at := func(row, cell int) float64 { return float64(out[row*cells+cell]) }
	scale := float64(max(w, h)) / side // the padded square, back from 320
	fw, fh := float64(w), float64(h)
	var found []detection
	for cell := range cells {
		label, score := 0, at(4, cell)
		for l := 1; l < len(labels); l++ {
			if s := at(4+l, cell); s > score {
				label, score = l, s
			}
		}
		if score < minScore {
			continue
		}
		bw, bh := at(2, cell), at(3, cell)
		x, y := (at(0, cell)-bw/2)*scale, (at(1, cell)-bh/2)*scale
		x, y = max(0, min(x, fw)), max(0, min(y, fh))
		bw, bh = min(bw*scale, fw-x), min(bh*scale, fh-y)
		found = append(found, detection{box: [4]float64{x, y, bw, bh}, score: score, label: label})
	}
	return suppress(found)
}

// suppress is OpenCV's NMSBoxes as NudeNet calls it: boxes over keepScore, surest first, each kept unless it
// overlaps one kept already by more than overlapSame, whatever the labels.
func suppress(all []detection) []detection {
	var candidates []detection
	for _, d := range all {
		if d.score > keepScore {
			candidates = append(candidates, d)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })
	var kept []detection
	for _, d := range candidates {
		same := false
		for _, k := range kept {
			if iou(d.box, k.box) > overlapSame {
				same = true
				break
			}
		}
		if !same {
			kept = append(kept, d)
		}
	}
	return kept
}

// answer is the highest score of the exposed parts found, and which they are.
func answer(found []detection) Result {
	var r Result
	parts := map[string]bool{}
	for _, d := range found {
		if label := labels[d.label]; exposed[label] {
			r.Score = max(r.Score, d.score)
			parts[label] = true
		}
	}
	for label := range parts {
		r.Found = append(r.Found, label)
	}
	sort.Strings(r.Found)
	return r
}

// iou is how much boxes a and b (x, y, w, h) overlap: the area they share over the area they cover.
func iou(a, b [4]float64) float64 {
	x0, y0 := max(a[0], b[0]), max(a[1], b[1])
	x1, y1 := min(a[0]+a[2], b[0]+b[2]), min(a[1]+a[3], b[1]+b[3])
	if x1 <= x0 || y1 <= y0 {
		return 0
	}
	shared := (x1 - x0) * (y1 - y0)
	return shared / (a[2]*a[3] + b[2]*b[3] - shared)
}

func toRGBA(img image.Image) *image.RGBA {
	if rgba, ok := img.(*image.RGBA); ok && rgba.Bounds().Min == (image.Point{}) {
		return rgba
	}
	b := img.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(rgba, rgba.Bounds(), img, b.Min, draw.Src)
	return rgba
}
