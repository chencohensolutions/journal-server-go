package faces

import (
	"image"
	"math"
	"sort"

	xdraw "golang.org/x/image/draw"

	"github.com/journal/server-go/onnx"
)

// YuNet (2023mar) reads a 640×640 picture: blue, green and red planes of 0 to 255, as OpenCV's FaceDetectorYN gives
// it. It answers, for each cell of a grid at three scales (every 8, 16 and 32 pixels), how sure it is of a face
// there (cls times obj), the box (offset from the cell and log size, in cells) and the five points (offsets, in cells).
const detectorSide = 640

var detectorStrides = [3]int{8, 16, 32}

var detectorOutputs = []string{
	"cls_8", "cls_16", "cls_32",
	"obj_8", "obj_16", "obj_32",
	"bbox_8", "bbox_16", "bbox_32",
	"kps_8", "kps_16", "kps_32",
}

const (
	// MinScore is how sure the detector must be to keep a face at all; OpenCV's demo uses 0.6 too.
	MinScore = 0.6
	// Boxes that overlap more than this (intersection over union) are one face; the surest is kept.
	overlapSame = 0.3
)

// detection is a face found, in pixels of the picture searched.
type detection struct {
	box       [4]float32 // x, y, w, h
	score     float32
	landmarks [10]float32
}

// detect finds the faces in img. YuNet takes 640×640 only, so the whole picture is searched shrunk to 640 (for the
// big faces), and then, when it is bigger, in overlapping 640 px tiles at its own size (for the small ones in the
// background). A face cut by a tile's inner edge is left to the tile beside it or to the whole picture.
func (m *Model) detect(img *image.RGBA) ([]detection, error) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	side := max(w, h)
	var all []detection

	scale := min(1, float32(detectorSide)/float32(side))
	small := img
	if scale < 1 {
		small = image.NewRGBA(image.Rect(0, 0, max(1, int(float32(w)*scale)), max(1, int(float32(h)*scale))))
		xdraw.ApproxBiLinear.Scale(small, small.Bounds(), img, img.Bounds(), xdraw.Src, nil)
	}
	whole, err := m.runDetector(small, small.Bounds())
	if err != nil {
		return nil, err
	}
	for _, d := range whole {
		for i := range d.box {
			d.box[i] /= scale
		}
		for i := range d.landmarks {
			d.landmarks[i] /= scale
		}
		all = append(all, d)
	}

	if side > detectorSide {
		for _, y0 := range tileStarts(h) {
			for _, x0 := range tileStarts(w) {
				tile := image.Rect(x0, y0, min(w, x0+detectorSide), min(h, y0+detectorSide))
				found, err := m.runDetector(img, tile)
				if err != nil {
					return nil, err
				}
				for _, d := range found {
					if cutByTile(d.box, tile, w, h) {
						continue
					}
					all = append(all, d)
				}
			}
		}
	}
	return suppress(all), nil
}

// tileStarts is where the 640 px tiles over a side of length n start: at both ends and in the middle, so that
// neighbours overlap by at least half a tile and any face up to 320 px is whole in one of them.
func tileStarts(n int) []int {
	if n <= detectorSide {
		return []int{0}
	}
	starts := []int{0, (n - detectorSide) / 2, n - detectorSide}
	if starts[1] == 0 {
		return []int{0, n - detectorSide}
	}
	return starts
}

// cutByTile reports whether box (in the picture's pixels) touches an edge of tile that is not the picture's own.
func cutByTile(box [4]float32, tile image.Rectangle, w, h int) bool {
	const margin = 2
	x0, y0, x1, y1 := box[0], box[1], box[0]+box[2], box[1]+box[3]
	return (tile.Min.X > 0 && x0 < float32(tile.Min.X+margin)) ||
		(tile.Min.Y > 0 && y0 < float32(tile.Min.Y+margin)) ||
		(tile.Max.X < w && x1 > float32(tile.Max.X-margin)) ||
		(tile.Max.Y < h && y1 > float32(tile.Max.Y-margin))
}

// runDetector runs YuNet on the part r of img (at most 640×640; the rest of the square is black) and returns what
// it found, in img's pixels.
func (m *Model) runDetector(img *image.RGBA, r image.Rectangle) ([]detection, error) {
	const plane = detectorSide * detectorSide
	pixels := make([]float32, 3*plane)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		row := img.Pix[img.PixOffset(r.Min.X, y):]
		for x := range r.Dx() {
			i := (y-r.Min.Y)*detectorSide + x
			pixels[i] = float32(row[4*x+2])         // blue
			pixels[plane+i] = float32(row[4*x+1])   // green
			pixels[2*plane+i] = float32(row[4*x+0]) // red
		}
	}
	out, err := onnx.RunNamed(m.detector, m.detector.Inputs[0],
		onnx.Tensor[float32]{Data: pixels, Shape: []int64{1, 3, detectorSide, detectorSide}}, detectorOutputs...)
	if err != nil {
		return nil, err
	}
	var found []detection
	for s, stride := range detectorStrides {
		cls, obj, bbox, kps := out[s].Data, out[3+s].Data, out[6+s].Data, out[9+s].Data
		cols := detectorSide / stride
		for idx := range cls {
			score := float32(math.Sqrt(float64(clamp01(cls[idx]) * clamp01(obj[idx]))))
			if score < MinScore {
				continue
			}
			row, col := float32(idx/cols), float32(idx%cols)
			st := float32(stride)
			cx, cy := (col+bbox[4*idx])*st, (row+bbox[4*idx+1])*st
			bw := float32(math.Exp(float64(bbox[4*idx+2]))) * st
			bh := float32(math.Exp(float64(bbox[4*idx+3]))) * st
			d := detection{box: [4]float32{cx - bw/2 + float32(r.Min.X), cy - bh/2 + float32(r.Min.Y), bw, bh}, score: score}
			for n := range 5 {
				d.landmarks[2*n] = (kps[10*idx+2*n]+col)*st + float32(r.Min.X)
				d.landmarks[2*n+1] = (kps[10*idx+2*n+1]+row)*st + float32(r.Min.Y)
			}
			found = append(found, d)
		}
	}
	return found, nil
}

// suppress keeps, of boxes that overlap more than overlapSame, the surest.
func suppress(all []detection) []detection {
	sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })
	var kept []detection
	for _, d := range all {
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

// iou is how much boxes a and b (x, y, w, h) overlap: the area they share over the area they cover.
func iou[T float32 | float64](a, b [4]T) T {
	x0, y0 := max(a[0], b[0]), max(a[1], b[1])
	x1, y1 := min(a[0]+a[2], b[0]+b[2]), min(a[1]+a[3], b[1]+b[3])
	if x1 <= x0 || y1 <= y0 {
		return 0
	}
	shared := (x1 - x0) * (y1 - y0)
	return shared / (a[2]*a[3] + b[2]*b[3] - shared)
}

func clamp01(v float32) float32 { return min(1, max(0, v)) }
