package faces

import (
	"image"
	"math"

	"github.com/journal/server-go/media"
)

// SFace reads a face 112×112, turned and scaled so that its five points sit where ArcFace's training put them (the
// template OpenCV's FaceRecognizerSF.alignCrop uses).
const alignedSide = 112

var template = [10]float32{
	38.2946, 51.6963, // the person's right eye (on the left of the picture)
	73.5318, 51.5014, // left eye
	56.0252, 71.7366, // nose tip
	41.5493, 92.3655, // right mouth corner
	70.7299, 92.2041, // left mouth corner
}

// similarity is a turn, a scale and a move: (x, y) → (a·x − b·y + tx, b·x + a·y + ty).
type similarity struct{ a, b, tx, ty float32 }

// fit is the similarity that best takes the points from (x, y pairs) onto to, by least squares.
func fit(from, to [10]float32) similarity {
	var fx, fy, tx, ty float32
	for i := range 5 {
		fx += from[2*i]
		fy += from[2*i+1]
		tx += to[2*i]
		ty += to[2*i+1]
	}
	fx, fy, tx, ty = fx/5, fy/5, tx/5, ty/5
	var dotSum, crossSum, norm float32
	for i := range 5 {
		px, py := from[2*i]-fx, from[2*i+1]-fy
		qx, qy := to[2*i]-tx, to[2*i+1]-ty
		dotSum += px*qx + py*qy
		crossSum += px*qy - py*qx
		norm += px*px + py*py
	}
	if norm == 0 {
		return similarity{a: 1, tx: tx - fx, ty: ty - fy}
	}
	a, b := dotSum/norm, crossSum/norm
	return similarity{a: a, b: b, tx: tx - (a*fx - b*fy), ty: ty - (b*fx + a*fy)}
}

func (s similarity) apply(x, y float32) (float32, float32) {
	return s.a*x - s.b*y + s.tx, s.b*x + s.a*y + s.ty
}

// inverse undoes s.
func (s similarity) inverse() similarity {
	d := s.a*s.a + s.b*s.b
	a, b := s.a/d, -s.b/d
	return similarity{a: a, b: b, tx: -(a*s.tx - b*s.ty), ty: -(b*s.tx + a*s.ty)}
}

// align cuts the face with the five points landmarks (in img's pixels) out of img, turned and scaled onto the
// template, as SFace reads it: red, green and blue planes of 0 to 255, bilinear, black outside the picture.
func align(img *image.RGBA, landmarks [10]float32) []float32 {
	back := fit(landmarks, template).inverse()
	const plane = alignedSide * alignedSide
	pixels := make([]float32, 3*plane)
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	for y := range alignedSide {
		for x := range alignedSide {
			sx, sy := back.apply(float32(x), float32(y))
			x0, y0 := int(floor(sx)), int(floor(sy))
			fx, fy := sx-float32(x0), sy-float32(y0)
			var rgb [3]float32
			for _, c := range [4]struct {
				dx, dy int
				weight float32
			}{{0, 0, (1 - fx) * (1 - fy)}, {1, 0, fx * (1 - fy)}, {0, 1, (1 - fx) * fy}, {1, 1, fx * fy}} {
				px, py := x0+c.dx, y0+c.dy
				if px < 0 || py < 0 || px >= w || py >= h || c.weight == 0 {
					continue
				}
				o := img.PixOffset(px, py)
				for k := range 3 {
					rgb[k] += c.weight * float32(img.Pix[o+k])
				}
			}
			i := y*alignedSide + x
			pixels[i], pixels[plane+i], pixels[2*plane+i] = rgb[0], rgb[1], rgb[2]
		}
	}
	return pixels
}

func floor(v float32) float32 {
	i := float32(int(v))
	if i > v {
		i--
	}
	return i
}

// The template's distance between the eyes, in its pixels.
var templateEyes = float32(math.Hypot(float64(template[2]-template[0]), float64(template[3]-template[1])))

// alignFromOriginal is the face d (found in a w×h picture of the photo) aligned for SFace from a part of the original
// cut by region: around the face with a margin, at the scale where its eyes are as far apart as the template's, so
// the alignment only turns it. false when there is no region, or it can't be cut.
func alignFromOriginal(region Region, d detection, w, h int) ([]float32, bool) {
	eyes := float32(math.Hypot(float64(d.landmarks[2]-d.landmarks[0]), float64(d.landmarks[3]-d.landmarks[1])))
	if region == nil || eyes < 1 {
		return nil, false
	}
	// A square 2.5 times the face, kept inside the picture.
	side := 2.5 * max(d.box[2], d.box[3])
	cx, cy := d.box[0]+d.box[2]/2, d.box[1]+d.box[3]/2
	left, top := max(0, cx-side/2), max(0, cy-side/2)
	right, bottom := min(float32(w), cx+side/2), min(float32(h), cy+side/2)
	if right-left < 1 || bottom-top < 1 {
		return nil, false
	}
	scale := templateEyes / eyes // the region's pixels per pixel of the picture
	outW := min(1024, max(1, int(math.Round(float64((right-left)*scale)))))
	outH := min(1024, max(1, int(math.Round(float64((bottom-top)*scale)))))
	crop, err := region(media.Rect{X: float64(left) / float64(w), Y: float64(top) / float64(h),
		W: float64(right-left) / float64(w), H: float64(bottom-top) / float64(h)}, outW, outH)
	if err != nil {
		return nil, false
	}
	// The five points in the crop's pixels.
	sx, sy := float32(outW)/(right-left), float32(outH)/(bottom-top)
	var points [10]float32
	for i := range 5 {
		points[2*i] = (d.landmarks[2*i] - left) * sx
		points[2*i+1] = (d.landmarks[2*i+1] - top) * sy
	}
	return align(crop, points), true
}
