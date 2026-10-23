// Package ocr reads the text in photos (screenshots, receipts, signs, documents) in Hebrew and English, with a
// PaddleOCR detector and recognizer finetuned for Hebrew (Rivok/paddleocr-hebrew, Apache 2.0) run by ONNX Runtime
// on the server's CPU. The detector finds each word in the photo fitted in 1280 px; each word is then cut from the
// original at the recognizer's height and read. The words are kept as lines in .cache/ocr/<model>.jsonl, and text
// search finds photos by them. See docs/indexes.md.
package ocr

import (
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/journal/server-go/media"
	"github.com/journal/server-go/onnx"
)

// The detector reads the photo fitted in 1280 px, as its card recommends (max_side 1280).
var Picture = media.PreviewSpec{Width: 1280, Height: 1280, Fit: "contain"}

// unclipRatio grows a word's box by its area × this / its perimeter: DBNet's boxes are shrunk in training. The
// model card says 1.3; 1.5 (PaddleOCR's default) keeps the last letter of a word that 1.3 cut off.
var unclipRatio = 1.5

// How words are found and read, as the model card recommends.
const (
	pixelThreshold = 0.3 // a pixel is text when the detector is this sure
	boxThreshold   = 0.35
	minSide        = 3   // pixels of the detection picture
	minHeight      = 10  // a box less high than this, in the 1280 px picture, holds no readable word (specks, icons)
	maxWords       = 200 // the biggest words of a photo that are read: a screenshot of a page has hundreds
	recHeight      = 48  // the recognizer's input: 48 px high, up to 320 wide
	recMaxWidth    = 320
	// Words read at once. One at a time is fastest on a CPU (Oct 23, 2026: 16 at once took 11 s, one by one 6 s).
	recBatch     = 1
	minWordScore = 0.5 // a word read less surely than this is dropped
)

// Model is the detector and recognizer, in a folder made by scripts/bundle-onnx: det.onnx, rec.onnx and
// charset.txt.
type Model struct {
	// The folder's name.
	Name string

	det, rec *onnx.Session
	// The recognizer's characters: index 0 is CTC's blank, then charset.txt's, then a space.
	chars []string
}

// LoadModel loads the models in folder, run with up to threads CPU threads each.
func LoadModel(rt *onnx.Runtime, folder string, threads int) (*Model, error) {
	abs, err := filepath.Abs(folder)
	if err != nil {
		return nil, err
	}
	m := &Model{Name: filepath.Base(abs)}
	data, err := os.ReadFile(filepath.Join(folder, "charset.txt"))
	if err != nil {
		return nil, err
	}
	m.chars = []string{""}
	for _, c := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		m.chars = append(m.chars, strings.TrimRight(c, "\r"))
	}
	m.chars = append(m.chars, " ")
	if m.det, err = rt.NewSession(filepath.Join(folder, "det.onnx"), threads); err != nil {
		return nil, err
	}
	if m.rec, err = rt.NewSession(filepath.Join(folder, "rec.onnx"), threads); err != nil {
		return nil, err
	}
	for _, s := range []*onnx.Session{m.det, m.rec} {
		if len(s.Inputs) != 1 || len(s.Outputs) != 1 {
			return nil, fmt.Errorf("an OCR model takes %v and gives %v; expected one each", s.Inputs, s.Outputs)
		}
		if err := s.Use(s.Inputs, s.Outputs); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Line is a line of text found in a photo.
type Line struct {
	Text string `json:"text"`
	// x, y, w, h as fractions of the upright photo.
	Box [4]float32 `json:"box"`
	// How sure the recognizer was, 0 to 1 (the mean over the line's characters).
	Score float32 `json:"score"`
}

// word is a word the detector found, in the detection picture's pixels.
type word struct {
	box   image.Rectangle
	text  string
	score float32
}

// Region cuts a part of the photo's original (fractions of the upright photo) at width×height: reading.Source's.
type Region func(box media.Rect, width, height int) (*image.RGBA, error)

// Read finds and reads the text in a photo: picture is the photo fitted in 1280 px, made from the original; region
// cuts each word from the original itself (nil: from picture).
func (m *Model) Read(picture *image.RGBA, region Region) ([]Line, error) {
	boxes, err := m.detect(picture)
	if err != nil || len(boxes) == 0 {
		return nil, err
	}
	boxes = slices.DeleteFunc(boxes, func(b image.Rectangle) bool { return b.Dy() < minHeight })
	if len(boxes) > maxWords {
		slices.SortFunc(boxes, func(a, b image.Rectangle) int { return b.Dx()*b.Dy() - a.Dx()*a.Dy() })
		boxes = boxes[:maxWords]
	}
	pw, ph := picture.Bounds().Dx(), picture.Bounds().Dy()
	crops := make([]*image.RGBA, 0, len(boxes))
	var kept []image.Rectangle
	for _, b := range boxes {
		w := min(recMaxWidth, max(1, int(math.Ceil(float64(recHeight*b.Dx())/float64(b.Dy())))))
		var crop *image.RGBA
		if region != nil {
			crop, err = region(media.Rect{X: float64(b.Min.X) / float64(pw), Y: float64(b.Min.Y) / float64(ph),
				W: float64(b.Dx()) / float64(pw), H: float64(b.Dy()) / float64(ph)}, w, recHeight)
		}
		if region == nil || err != nil {
			crop = resize(picture, b, w, recHeight)
		}
		crops = append(crops, crop)
		kept = append(kept, b)
	}
	var words []word
	for start := 0; start < len(crops); start += recBatch {
		end := min(len(crops), start+recBatch)
		read, err := m.recognize(crops[start:end])
		if err != nil {
			return nil, err
		}
		for i, r := range read {
			if r.score >= minWordScore && strings.TrimSpace(r.text) != "" {
				words = append(words, word{box: kept[start+i], text: strings.TrimSpace(r.text), score: r.score})
			}
		}
	}
	return lines(words, pw, ph), nil
}

// detect is the boxes of the words in img: DBNet's map of how sure it is that each pixel is text, cut at
// pixelThreshold into blobs, each blob's box kept when the map's mean over it reaches boxThreshold, then grown
// (DBNet's boxes are shrunk in training).
func (m *Model) detect(img *image.RGBA) ([]image.Rectangle, error) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	// The detector takes sides that are multiples of 32: the picture is padded with black.
	pw, ph := (w+31)/32*32, (h+31)/32*32
	plane := pw * ph
	pixels := make([]float32, 3*plane)
	mean, std := [3]float32{0.485, 0.456, 0.406}, [3]float32{0.229, 0.224, 0.225}
	for c := range 3 {
		fill := -mean[c] / std[c]
		for i := c * plane; i < (c+1)*plane; i++ {
			pixels[i] = fill
		}
	}
	for y := range h {
		row := img.Pix[img.PixOffset(0, y):]
		for x := range w {
			// Blue, green, red: PaddleOCR reads pictures with OpenCV, and normalizes them in that order.
			for c, from := range [3]int{2, 1, 0} {
				pixels[c*plane+y*pw+x] = (float32(row[4*x+from])/255 - mean[c]) / std[c]
			}
		}
	}
	out, err := onnx.Run(m.det, onnx.Tensor[float32]{Data: pixels, Shape: []int64{1, 3, int64(ph), int64(pw)}})
	if err != nil {
		return nil, err
	}
	if len(out.Data) != plane {
		return nil, fmt.Errorf("det.onnx gave %v for a %d×%d picture", out.Shape, pw, ph)
	}
	prob := out.Data
	seen := make([]bool, plane)
	var boxes []image.Rectangle
	var stack []int
	for start := range plane {
		if seen[start] || prob[start] < pixelThreshold || start%pw >= w || start/pw >= h {
			continue
		}
		// One blob: its bounding box and the map's mean over its pixels.
		minX, minY, maxX, maxY := pw, ph, 0, 0
		var sum float64
		n := 0
		stack = append(stack[:0], start)
		seen[start] = true
		for len(stack) > 0 {
			p := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			x, y := p%pw, p/pw
			minX, minY, maxX, maxY = min(minX, x), min(minY, y), max(maxX, x), max(maxY, y)
			sum += float64(prob[p])
			n++
			for _, q := range [4]int{p - 1, p + 1, p - pw, p + pw} {
				if q < 0 || q >= plane || seen[q] || prob[q] < pixelThreshold || (q%pw == 0 && p%pw == pw-1) || (p%pw == 0 && q%pw == pw-1) {
					continue
				}
				seen[q] = true
				stack = append(stack, q)
			}
		}
		bw, bh := maxX-minX+1, maxY-minY+1
		if min(bw, bh) < minSide || sum/float64(n) < boxThreshold {
			continue
		}
		grow := int(math.Round(float64(bw*bh) * unclipRatio / float64(2*(bw+bh))))
		r := image.Rect(minX-grow, minY-grow, maxX+1+grow, maxY+1+grow).Intersect(image.Rect(0, 0, w, h))
		if r.Dx() >= minSide && r.Dy() >= minSide && r.Dy() <= 2*r.Dx() { // text written upright isn't read
			boxes = append(boxes, r)
		}
	}
	return boxes, nil
}

type read struct {
	text  string
	score float32 // the mean of its characters' probabilities
	least float32 // the least sure character's
}

// recognize reads each crop (48 px high): CTC, the most likely character at each step, repeats and blanks dropped.
func (m *Model) recognize(crops []*image.RGBA) ([]read, error) {
	width := 0
	for _, c := range crops {
		width = max(width, c.Bounds().Dx())
	}
	n, plane := len(crops), recHeight*width
	pixels := make([]float32, n*3*plane) // padding is 0: grey once normalized, as PaddleOCR pads
	for k, c := range crops {
		base := k * 3 * plane
		for y := range recHeight {
			row := c.Pix[c.PixOffset(0, y):]
			for x := range c.Bounds().Dx() {
				for ch, from := range [3]int{2, 1, 0} { // blue, green, red
					pixels[base+ch*plane+y*width+x] = float32(row[4*x+from])/127.5 - 1
				}
			}
		}
	}
	out, err := onnx.Run(m.rec, onnx.Tensor[float32]{Data: pixels, Shape: []int64{int64(n), 3, recHeight, int64(width)}})
	if err != nil {
		return nil, err
	}
	if len(out.Shape) != 3 || int(out.Shape[0]) != n || int(out.Shape[2]) != len(m.chars) {
		return nil, fmt.Errorf("rec.onnx gave %v, expected [%d, steps, %d]", out.Shape, n, len(m.chars))
	}
	steps, classes := int(out.Shape[1]), int(out.Shape[2])
	reads := make([]read, n)
	for k := range n {
		var text strings.Builder
		var sum float32
		least := float32(1)
		count, last := 0, 0
		for t := range steps {
			probs := out.Data[(k*steps+t)*classes : (k*steps+t+1)*classes]
			best := 0
			for c := range probs {
				if probs[c] > probs[best] {
					best = c
				}
			}
			if best != 0 && best != last {
				text.WriteString(m.chars[best])
				sum += probs[best]
				least = min(least, probs[best])
				count++
			}
			last = best
		}
		if count > 0 {
			reads[k] = read{text.String(), sum / float32(count), least}
		}
	}
	return reads, nil
}

// lines puts words into lines (by their overlap from top to bottom), each line's words in reading order: right to
// left when it is mostly Hebrew. Their boxes become fractions of the w×h picture.
func lines(words []word, w, h int) []Line {
	sort.Slice(words, func(i, j int) bool {
		return words[i].box.Min.Y+words[i].box.Max.Y < words[j].box.Min.Y+words[j].box.Max.Y
	})
	var groups [][]word
	for _, wd := range words {
		placed := false
		for i, g := range groups {
			last := g[len(g)-1].box
			overlap := min(last.Max.Y, wd.box.Max.Y) - max(last.Min.Y, wd.box.Min.Y)
			if overlap*2 >= min(last.Dy(), wd.box.Dy()) {
				groups[i] = append(groups[i], wd)
				placed = true
				break
			}
		}
		if !placed {
			groups = append(groups, []word{wd})
		}
	}
	var out []Line
	for _, g := range groups {
		rtl := hebrewMostly(g)
		slices.SortFunc(g, func(a, b word) int {
			if rtl {
				return b.box.Min.X - a.box.Min.X
			}
			return a.box.Min.X - b.box.Min.X
		})
		var texts []string
		var score float32
		box := g[0].box
		for _, wd := range g {
			texts = append(texts, wd.text)
			score += wd.score
			box = box.Union(wd.box)
		}
		out = append(out, Line{Text: strings.Join(texts, " "), Score: score / float32(len(g)),
			Box: [4]float32{float32(box.Min.X) / float32(w), float32(box.Min.Y) / float32(h),
				float32(box.Dx()) / float32(w), float32(box.Dy()) / float32(h)}})
	}
	return out
}

func hebrewMostly(words []word) bool {
	hebrew, latin := 0, 0
	for _, w := range words {
		for _, r := range w.text {
			switch {
			case unicode.Is(unicode.Hebrew, r):
				hebrew++
			case unicode.Is(unicode.Latin, r):
				latin++
			}
		}
	}
	return hebrew > latin
}

// resize is the part r of img at width×height (nearest neighbour is enough for a fallback).
func resize(img *image.RGBA, r image.Rectangle, width, height int) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		sy := r.Min.Y + y*r.Dy()/height
		for x := range width {
			sx := r.Min.X + x*r.Dx()/width
			copy(out.Pix[out.PixOffset(x, y):out.PixOffset(x, y)+4], img.Pix[img.PixOffset(sx, sy):img.PixOffset(sx, sy)+4])
		}
	}
	return out
}
