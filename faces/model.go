package faces

import (
	"fmt"
	"image"
	"image/draw"
	"math"
	"path/filepath"
	"slices"

	"github.com/journal/server-go/onnx"
)

// The files of the two models, as scripts/bundle-onnx downloads them from OpenCV's model zoo.
const (
	detectorFile   = "face_detection_yunet_2023mar.onnx"
	recognizerFile = "face_recognition_sface_2021dec.onnx"
)

// Dim is how many numbers a face's vector has.
const Dim = 128

// Model is the two face models, in a folder made by scripts/bundle-onnx: YuNet finds the faces in a picture (a box,
// how sure it is, and five points: the eyes, the nose and the corners of the mouth), and SFace turns each face,
// turned and scaled so the five points sit where it expects them, into a vector. Two faces of the same person point
// the same way: OpenCV, who trained and published both, take a cosine of 0.363 or more as the same person.
type Model struct {
	// The folder's name. Vectors of different models can't be compared, so each model keeps its own.
	Name string

	detector, recognizer *onnx.Session
}

// LoadModel loads the models in folder, run with up to threads CPU threads each.
func LoadModel(rt *onnx.Runtime, folder string, threads int) (*Model, error) {
	abs, err := filepath.Abs(folder)
	if err != nil {
		return nil, err
	}
	m := &Model{Name: filepath.Base(abs)}
	if m.detector, err = rt.NewSession(filepath.Join(folder, detectorFile), threads); err != nil {
		return nil, err
	}
	for _, name := range detectorOutputs {
		if !slices.Contains(m.detector.Outputs, name) {
			return nil, fmt.Errorf("%s gives %v, not %s", detectorFile, m.detector.Outputs, name)
		}
	}
	if m.recognizer, err = rt.NewSession(filepath.Join(folder, recognizerFile), threads); err != nil {
		return nil, err
	}
	if err := m.recognizer.Use([]string{"data"}, []string{"fc1"}); err != nil {
		return nil, fmt.Errorf("%s: %w", recognizerFile, err)
	}
	return m, nil
}

// Face is a face found in a picture. Box and Landmarks are fractions of the picture's width and height, so they
// hold for the original and any preview of it.
type Face struct {
	// x, y (top left), width and height.
	Box [4]float32
	// How sure the detector is that it is a face, 0 to 1.
	Score float32
	// The person's right eye, left eye, nose tip, right and left mouth corners, as x, y pairs.
	Landmarks [10]float32
	// The face's vector, of length 1.
	Vector []float32
}

// Faces finds the faces in img and reads each into a vector.
func (m *Model) Faces(img image.Image) ([]Face, error) {
	rgba := toRGBA(img)
	found, err := m.detect(rgba)
	if err != nil {
		return nil, err
	}
	w, h := float32(rgba.Bounds().Dx()), float32(rgba.Bounds().Dy())
	faces := make([]Face, 0, len(found))
	for _, d := range found {
		vector, err := m.vector(align(rgba, d.landmarks))
		if err != nil {
			return nil, err
		}
		f := Face{Box: [4]float32{d.box[0] / w, d.box[1] / h, d.box[2] / w, d.box[3] / h}, Score: d.score, Vector: vector}
		for i := range 5 {
			f.Landmarks[2*i], f.Landmarks[2*i+1] = d.landmarks[2*i]/w, d.landmarks[2*i+1]/h
		}
		faces = append(faces, f)
	}
	return faces, nil
}

// vector runs SFace on an aligned face: 112×112, red, green and blue planes of 0 to 255.
func (m *Model) vector(pixels []float32) ([]float32, error) {
	out, err := onnx.Run(m.recognizer, onnx.Tensor[float32]{Data: pixels, Shape: []int64{1, 3, alignedSide, alignedSide}})
	if err != nil {
		return nil, err
	}
	if len(out.Data) != Dim {
		return nil, fmt.Errorf("%s gave %d numbers, not %d", recognizerFile, len(out.Data), Dim)
	}
	return unit(out.Data), nil
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

// unit scales v to length 1, so that the dot product of two vectors is their cosine similarity.
func unit(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	scale := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= scale
	}
	return v
}

func dot(a, b []float32) float32 {
	var sum float32
	for i, x := range a {
		sum += x * b[i]
	}
	return sum
}
