// Package aesthetic scores how good each photo looks, with LAION's aesthetic predictor (the community's standard:
// christophschuhmann/improved-aesthetic-predictor, Apache 2.0) on OpenAI's CLIP ViT-L/14 (MIT), run by ONNX Runtime
// on the server's CPU. CLIP turns the photo into a vector; LAION's head, five linear layers that scripts/bundle-onnx
// multiplies out into one, turns that into a score of about 1 to 10, trained on people's ratings: around 5 is an
// ordinary photo, 6 and more a good one. Each file is read once (package reading: the original's centre at 224 px;
// a video's poster frame) and its score kept in .cache/aesthetic/<model>.vectors. See docs/indexes.md.
package aesthetic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"

	"github.com/journal/server-go/media"
	"github.com/journal/server-go/onnx"
	"github.com/journal/server-go/reading"
	"github.com/journal/server-go/search"
)

// Model is CLIP ViT-L/14's vision half and LAION's head, in a folder made by scripts/bundle-onnx:
// vision_model.onnx, preprocessor_config.json and laion-head.json.
type Model struct {
	// The folder's name.
	Name string

	session   *onnx.Session
	size      int
	mean, std [3]float32
	weights   []float32 // LAION's head as one layer: score = weights · the unit embedding + bias
	bias      float32
}

// LoadModel loads the model in folder, run with up to threads CPU threads.
func LoadModel(rt *onnx.Runtime, folder string, threads int) (*Model, error) {
	abs, err := filepath.Abs(folder)
	if err != nil {
		return nil, err
	}
	m := &Model{Name: filepath.Base(abs)}
	data, err := os.ReadFile(filepath.Join(folder, "preprocessor_config.json"))
	if err != nil {
		return nil, err
	}
	var p struct {
		Crop struct{ Width, Height int } `json:"crop_size"`
		Mean []float32                   `json:"image_mean"`
		Std  []float32                   `json:"image_std"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("preprocessor_config.json: %w", err)
	}
	if p.Crop.Width < 1 || p.Crop.Width != p.Crop.Height || len(p.Mean) != 3 || len(p.Std) != 3 {
		return nil, errors.New("preprocessor_config.json: expected a square crop and three means and deviations")
	}
	m.size = p.Crop.Width
	copy(m.mean[:], p.Mean)
	copy(m.std[:], p.Std)
	if data, err = os.ReadFile(filepath.Join(folder, "laion-head.json")); err != nil {
		return nil, err
	}
	var head struct {
		Weights []float32 `json:"weights"`
		Bias    float32   `json:"bias"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("laion-head.json: %w", err)
	}
	m.weights, m.bias = head.Weights, head.Bias
	if m.session, err = rt.NewSession(filepath.Join(folder, "vision_model.onnx"), threads); err != nil {
		return nil, err
	}
	if err := m.session.Use([]string{"pixel_values"}, []string{"image_embeds"}); err != nil {
		return nil, fmt.Errorf("vision_model.onnx: %w", err)
	}
	if _, err := m.Score(image.NewRGBA(image.Rect(0, 0, m.size, m.size))); err != nil {
		return nil, fmt.Errorf("vision_model.onnx: %w", err)
	}
	return m, nil
}

// Picture is what the model reads of a file: the original's centre at the model's square, as CLIP was trained
// (its short side scaled to 224, the centre cut).
func (m *Model) Picture() media.PreviewSpec {
	return media.PreviewSpec{Width: m.size, Height: m.size, Fit: "cover"}
}

// Score is a picture's aesthetic score, about 1 to 10.
func (m *Model) Score(img *image.RGBA) (float32, error) {
	b := img.Bounds()
	if b.Dx() != m.size || b.Dy() != m.size {
		return 0, fmt.Errorf("a %d×%d picture, the model reads %d×%d", b.Dx(), b.Dy(), m.size, m.size)
	}
	plane := m.size * m.size
	pixels := make([]float32, 3*plane)
	for i := range plane {
		for c := range 3 {
			v := float32(img.Pix[4*i+c]) / 255
			pixels[c*plane+i] = (v - m.mean[c]) / m.std[c]
		}
	}
	size := int64(m.size)
	out, err := onnx.Run(m.session, onnx.Tensor[float32]{Data: pixels, Shape: []int64{1, 3, size, size}})
	if err != nil {
		return 0, err
	}
	if len(out.Data) != len(m.weights) {
		return 0, fmt.Errorf("vision_model.onnx gave %d numbers, the head takes %d", len(out.Data), len(m.weights))
	}
	// LAION's head reads the embedding scaled to length 1.
	var norm float64
	for _, v := range out.Data {
		norm += float64(v) * float64(v)
	}
	if norm == 0 {
		return m.bias, nil
	}
	scale := float32(1 / math.Sqrt(norm))
	score := m.bias
	for i, v := range out.Data {
		score += m.weights[i] * v * scale
	}
	return score, nil
}

// Service is the users' photos' aesthetic scores: an index of package reading.
type Service struct {
	model   *Model
	library *media.Library
	scores  map[string]*search.Index // a vector of one number per file, by username
}

// NewService opens the model's scores in the data folder of each of library's users.
func NewService(model *Model, library *media.Library) (*Service, error) {
	s := &Service{model: model, library: library, scores: map[string]*search.Index{}}
	for _, user := range library.Users() {
		index, err := search.OpenVectors(filepath.Join(library.Store(user).Root(), ".cache", "aesthetic", model.Name+".vectors"), 1)
		if err != nil {
			return nil, fmt.Errorf("user %s: %w", user, err)
		}
		s.scores[user] = index
	}
	return s, nil
}

// NewReadOnlyService answers queries about the scores another process gives (journal-server index), named after
// its model: each user's scores opened to read only. Refresh reads what that process added.
func NewReadOnlyService(model string, library *media.Library) (*Service, error) {
	s := &Service{model: &Model{Name: model}, library: library, scores: map[string]*search.Index{}}
	for _, user := range library.Users() {
		index, err := search.OpenVectorsReadOnly(filepath.Join(library.Store(user).Root(), ".cache", "aesthetic", model+".vectors"), 1)
		if err != nil {
			return nil, fmt.Errorf("user %s: %w", user, err)
		}
		s.scores[user] = index
	}
	return s, nil
}

// Refresh reads the scores another process gave since the last look (NewReadOnlyService).
func (s *Service) Refresh() error {
	for user, index := range s.scores {
		if _, err := index.Refresh(); err != nil {
			return fmt.Errorf("user %s: %w", user, err)
		}
	}
	return nil
}

// Model is the name of the model.
func (s *Service) Model() string { return s.model.Name }

func (s *Service) Name() string { return "aesthetic" }

// Order: last, the slowest and least urgent.
func (s *Service) Order() int { return 30 }

func (s *Service) Wants() reading.Wants {
	return reading.Wants{Kinds: []media.Kind{media.KindPhoto, media.KindVideo}, Pictures: []media.PreviewSpec{s.model.Picture()}}
}

func (s *Service) Needs(user string, item media.Item) bool {
	index := s.scores[user]
	return index != nil && !index.Has(item.Sha256())
}

func (s *Service) Read(ctx context.Context, user string, in reading.Input) error {
	score, err := s.model.Score(in.Pictures[s.model.Picture()])
	if err != nil {
		return err
	}
	return s.scores[user].Add(in.Item.Sha256(), []float32{score})
}

// Carry gives the file to the score of the file from, whose pictures are the same (a JPEG kept as JPEG XL), so it
// isn't read again; false when from has none.
func (s *Service) Carry(user, from, to string) (bool, error) {
	index := s.scores[user]
	if index == nil {
		return false, nil
	}
	score := index.Vector(from)
	if score == nil {
		return false, nil
	}
	return true, index.Add(to, score)
}

func (s *Service) Finish(user string) {}

func (s *Service) FailureLog() (folder, name string) {
	return "aesthetic", s.model.Name + ".failed.log"
}

// Score is user's file sha's aesthetic score, and whether it has been read.
func (s *Service) Score(user, sha string) (float32, bool) {
	if index := s.scores[user]; index != nil {
		if v := index.Vector(sha); v != nil {
			return v[0], true
		}
	}
	return 0, false
}

// CacheFile is user's scores file (a vectors file of one number a file); "" for a user with no data folder.
func (s *Service) CacheFile(user string) string {
	if index := s.scores[user]; index != nil {
		return index.Path()
	}
	return ""
}

// Has reports whether user's file sha has been scored.
func (s *Service) Has(user, sha string) bool {
	index := s.scores[user]
	return index != nil && index.Has(sha)
}

// Record is user's file sha's record as the scores file keeps it; nil when it hasn't been scored.
func (s *Service) Record(user, sha string) []byte {
	if index := s.scores[user]; index != nil {
		return index.Record(sha)
	}
	return nil
}

// Counts is how many of user's files have been scored, of all of them; files hide reports true for are left out.
func (s *Service) Counts(user string, hide func(media.Item) bool) (read, total int) {
	index, store := s.scores[user], s.library.Store(user)
	if index == nil {
		return 0, 0
	}
	store.Range(func(item media.Item) {
		if hide(item) {
			return
		}
		total++
		if index.Has(item.Sha256()) {
			read++
		}
	})
	return read, total
}

// Close closes the users' score files.
func (s *Service) Close() {
	for _, index := range s.scores {
		index.Close()
	}
}
