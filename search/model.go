package search

import (
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/eliben/go-sentencepiece"
	"github.com/journal/server-go/onnx"
	"golang.org/x/image/draw"
)

// SigLIP 2 reads text as 64 tokens: the words, then <eos>, then <pad> to fill.
const textLength = 64

// PhoneTextModelQ8 is the 8-bit text half that bundle-onnx puts beside the full-precision model, for phones that
// search themselves (a quarter of text_model.onnx's size); the server never loads it. Its vectors are close to the
// full model's, so they rank the photos' vectors (read with the full vision half) about the same (TestPhoneTextModel).
const PhoneTextModelQ8 = "text_model_q8.onnx"

// Model is a SigLIP 2 model, in a folder made by scripts/bundle-onnx. It has two halves: the vision half turns a
// photo into a vector, the text half turns a description into a vector in the same space, and the better the text
// describes the photo, the closer the two point.
type Model struct {
	// The folder's name. Vectors of different models can't be compared, so each model keeps its own.
	Name string
	// How many numbers a vector has: 768 for the base model, 1152 for the large one.
	Dim int

	vision, text *onnx.Session
	tokenizer    *sentencepiece.Processor
	eos, pad     int

	// The picture the vision half takes: Sizeֳ—Size, each channel scaled to (value/255 - mean) / std.
	size      int
	mean, std [3]float32
}

// preprocessor is the part of preprocessor_config.json that says how pictures are prepared.
type preprocessor struct {
	Size      struct{ Width, Height int } `json:"size"`
	ImageMean []float32                   `json:"image_mean"`
	ImageStd  []float32                   `json:"image_std"`
}

// LoadModel loads the model in folder, run with up to threads CPU threads per half.
func LoadModel(rt *onnx.Runtime, folder string, threads int) (*Model, error) {
	return loadModel(rt, folder, threads, true, "text_model.onnx")
}

// LoadTextModel loads only the text half of the model in folder: what searches need, for a server whose files
// are read by another process (journal-server index). ImageVector then fails.
func LoadTextModel(rt *onnx.Runtime, folder string, threads int) (*Model, error) {
	return loadModel(rt, folder, threads, false, "text_model.onnx")
}

// LoadPhoneTextModel loads the text half phones may run instead (PhoneTextModelQ8, beside the model in folder): to
// check that it ranks as the full one does, and to give the apps vectors to test against (scripts/local-fixtures).
func LoadPhoneTextModel(rt *onnx.Runtime, folder string, threads int) (*Model, error) {
	return loadModel(rt, folder, threads, false, PhoneTextModelQ8)
}

func loadModel(rt *onnx.Runtime, folder string, threads int, vision bool, textFile string) (*Model, error) {
	abs, err := filepath.Abs(folder)
	if err != nil {
		return nil, err
	}
	m := &Model{Name: filepath.Base(abs)}

	data, err := os.ReadFile(filepath.Join(folder, "preprocessor_config.json"))
	if err != nil {
		return nil, err
	}
	var p preprocessor
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("preprocessor_config.json: %w", err)
	}
	if p.Size.Width < 1 || p.Size.Width != p.Size.Height || len(p.ImageMean) != 3 || len(p.ImageStd) != 3 {
		return nil, errors.New("preprocessor_config.json: expected a square size and three means and deviations")
	}
	m.size = p.Size.Width
	copy(m.mean[:], p.ImageMean)
	copy(m.std[:], p.ImageStd)

	if m.tokenizer, err = sentencepiece.NewProcessorFromPath(filepath.Join(folder, "tokenizer.model")); err != nil {
		return nil, fmt.Errorf("tokenizer.model: %w", err)
	}
	info := m.tokenizer.ModelInfo()
	m.eos, m.pad = info.EndOfSentenceID, info.PadID
	if m.eos < 0 || m.pad < 0 {
		return nil, errors.New("tokenizer.model has no <eos> or <pad>")
	}

	// pooler_output is the embedding: the halves end in their projection heads.
	if vision {
		if m.vision, err = rt.NewSession(filepath.Join(folder, "vision_model.onnx"), threads); err != nil {
			return nil, err
		}
		if err := m.vision.Use([]string{"pixel_values"}, []string{"pooler_output", "image_embeds"}); err != nil {
			return nil, fmt.Errorf("vision_model.onnx: %w", err)
		}
	}
	if m.text, err = rt.NewSession(filepath.Join(folder, textFile), threads); err != nil {
		return nil, err
	}
	if err := m.text.Use([]string{"input_ids"}, []string{"pooler_output", "text_embeds"}); err != nil {
		return nil, fmt.Errorf("text_model.onnx: %w", err)
	}
	// The length of the vectors, from a first run (which also has ONNX Runtime prepare it).
	v, err := m.TextVector("")
	if err != nil {
		return nil, fmt.Errorf("text_model.onnx: %w", err)
	}
	m.Dim = len(v)
	return m, nil
}

// ImageVector is the vector of a picture, of length 1. The picture is squeezed to the model's square (as SigLIP
// was trained, not cut), with bilinear resampling.
func (m *Model) ImageVector(img image.Image) ([]float32, error) {
	if m.vision == nil {
		return nil, errors.New("only the model's text half is loaded")
	}
	square := image.NewRGBA(image.Rect(0, 0, m.size, m.size))
	draw.BiLinear.Scale(square, square.Bounds(), img, img.Bounds(), draw.Src, nil)
	plane := m.size * m.size
	pixels := make([]float32, 3*plane)
	for i := range plane {
		for c := range 3 {
			v := float32(square.Pix[4*i+c]) / 255
			pixels[c*plane+i] = (v - m.mean[c]) / m.std[c]
		}
	}
	size := int64(m.size)
	out, err := onnx.Run(m.vision, onnx.Tensor[float32]{Data: pixels, Shape: []int64{1, 3, size, size}})
	if err != nil {
		return nil, err
	}
	return unit(out.Data), nil
}

// Close releases the model's halves and the memory they hold; the model can't be used after.
func (m *Model) Close() {
	if m.vision != nil {
		m.vision.Close()
	}
	if m.text != nil {
		m.text.Close()
	}
}

// TextVector is the vector of a description, of length 1.
func (m *Model) TextVector(text string) ([]float32, error) {
	return m.textVector(m.Tokens(text))
}

func (m *Model) textVector(tokens []int64) ([]float32, error) {
	out, err := onnx.Run(m.text, onnx.Tensor[int64]{Data: tokens, Shape: []int64{1, textLength}})
	if err != nil {
		return nil, err
	}
	return unit(out.Data), nil
}

// Tokens is text as the text half reads it: lowercased (SigLIP 2 was trained on lowercase text), cut to fit,
// then <eos> and <pad> up to textLength.
func (m *Model) Tokens(text string) []int64 {
	tokens := make([]int64, 0, textLength)
	for _, t := range m.tokenizer.Encode(strings.ToLower(text)) {
		if len(tokens) == textLength-1 {
			break
		}
		tokens = append(tokens, int64(t.ID))
	}
	tokens = append(tokens, int64(m.eos))
	for len(tokens) < textLength {
		tokens = append(tokens, int64(m.pad))
	}
	return tokens
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
