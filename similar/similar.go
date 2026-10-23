// Package similar finds photos that look like a given one, and near-identical ones (duplicates), with Meta's DINOv2
// (small) run by ONNX Runtime on the server's CPU. DINOv2 compares how photos look, not what they show: the same
// scene shot again, an edit, a copy sent through WhatsApp. Each file is read once (package reading: the original
// cropped to the model's square; a video's poster frame) into a vector kept in .cache/similar/<model>.vectors. See
// docs/indexes.md.
package similar

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"

	"github.com/journal/server-go/media"
	"github.com/journal/server-go/onnx"
	"github.com/journal/server-go/reading"
	"github.com/journal/server-go/search"
)

// DuplicateSimilarity is how alike two photos' vectors must be (cosine) to be duplicates: copies, not just the same
// scene. Measured on the owner's photos (Oct 23, 2026): a photo and a smaller, recompressed copy of it (as WhatsApp
// sends) score 0.974 to 0.993; the same scene shot again seconds later 0.90 to 0.95, which text search already
// keeps together as a set; different photos 0.07 to 0.86.
var DuplicateSimilarity float32 = 0.96

// Model is DINOv2, in a folder made by scripts/bundle-onnx: model.onnx and preprocessor_config.json.
type Model struct {
	// The folder's name. Vectors of different models can't be compared, so each model keeps its own.
	Name string
	// How many numbers a vector has: 384 for the small model.
	Dim int

	session   *onnx.Session
	size      int
	mean, std [3]float32
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
	if m.session, err = rt.NewSession(filepath.Join(folder, "model.onnx"), threads); err != nil {
		return nil, err
	}
	if err := m.session.Use([]string{"pixel_values"}, []string{"last_hidden_state"}); err != nil {
		return nil, fmt.Errorf("model.onnx: %w", err)
	}
	// The length of the vectors, from a first run (which also has ONNX Runtime prepare it).
	v, err := m.Vector(image.NewRGBA(image.Rect(0, 0, m.size, m.size)))
	if err != nil {
		return nil, fmt.Errorf("model.onnx: %w", err)
	}
	m.Dim = len(v)
	return m, nil
}

// Picture is what the model reads of a file: the original's centre, cropped to the model's square (as DINOv2 was
// trained, which scales the short side to 256 and cuts the centre 224; here the whole short side is kept).
func (m *Model) Picture() media.PreviewSpec {
	return media.PreviewSpec{Width: m.size, Height: m.size, Fit: "cover"}
}

// Vector is the vector of a picture of the model's size: its class token, of length 1.
func (m *Model) Vector(img *image.RGBA) ([]float32, error) {
	b := img.Bounds()
	if b.Dx() != m.size || b.Dy() != m.size {
		return nil, fmt.Errorf("a %d×%d picture, the model reads %d×%d", b.Dx(), b.Dy(), m.size, m.size)
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
		return nil, err
	}
	if len(out.Shape) != 3 || out.Shape[0] != 1 {
		return nil, fmt.Errorf("model.onnx gave %v", out.Shape)
	}
	dim := int(out.Shape[2])
	return unit(append([]float32(nil), out.Data[:dim]...)), nil // the class token, first of the sequence
}

// Service is similar photos and duplicates over the users' files: an index of package reading.
type Service struct {
	model   *Model
	library *media.Library
	users   map[string]*user
}

type user struct {
	index *search.Index
	// Duplicates found so far, and how many of the index's vectors they cover.
	mu      sync.Mutex
	groups  *unionFind
	checked int
}

// NewService opens the model's vectors in the data folder of each of library's users.
func NewService(model *Model, library *media.Library) (*Service, error) {
	s := &Service{model: model, library: library, users: map[string]*user{}}
	for _, name := range library.Users() {
		index, err := search.OpenVectors(filepath.Join(library.Store(name).Root(), ".cache", "similar", model.Name+".vectors"), model.Dim)
		if err != nil {
			return nil, fmt.Errorf("user %s: %w", name, err)
		}
		s.users[name] = &user{index: index, groups: newUnionFind()}
	}
	return s, nil
}

// NewReadOnlyService answers queries about the vectors another process makes (journal-server index), named after
// its model: each user's vectors opened to read only. Refresh reads what that process added.
func NewReadOnlyService(model string, library *media.Library) (*Service, error) {
	s := &Service{model: &Model{Name: model}, library: library, users: map[string]*user{}}
	for _, name := range library.Users() {
		index, err := search.OpenVectorsReadOnly(filepath.Join(library.Store(name).Root(), ".cache", "similar", model+".vectors"), 0)
		if err != nil {
			return nil, fmt.Errorf("user %s: %w", name, err)
		}
		s.users[name] = &user{index: index, groups: newUnionFind()}
	}
	return s, nil
}

// Refresh reads the vectors another process made since the last look (NewReadOnlyService), and finds the
// duplicates among the new files in the background, so the next duplicates query doesn't wait for them.
func (s *Service) Refresh() error {
	for name, u := range s.users {
		n, err := u.index.Refresh()
		if err != nil {
			return fmt.Errorf("user %s: %w", name, err)
		}
		if n > 0 {
			go u.findDuplicates()
		}
	}
	return nil
}

// Model is the name of the model.
func (s *Service) Model() string { return s.model.Name }

func (s *Service) Name() string { return "similar" }

// Order: after the nudity scan, faces and text search.
func (s *Service) Order() int { return 25 }

func (s *Service) Wants() reading.Wants {
	return reading.Wants{Kinds: []media.Kind{media.KindPhoto, media.KindVideo}, Pictures: []media.PreviewSpec{s.model.Picture()}}
}

func (s *Service) Needs(user string, item media.Item) bool {
	u := s.users[user]
	return u != nil && !u.index.Has(item.Sha256())
}

func (s *Service) Read(ctx context.Context, user string, in reading.Input) error {
	vector, err := s.model.Vector(in.Pictures[s.model.Picture()])
	if err != nil {
		return err
	}
	return s.users[user].index.Add(in.Item.Sha256(), vector)
}

// Finish finds the duplicates among the files just read and the others.
func (s *Service) Finish(user string) {
	if u := s.users[user]; u != nil {
		u.findDuplicates()
	}
}

func (s *Service) FailureLog() (folder, name string) { return "similar", s.model.Name + ".failed.log" }

// Hit is a file found like another, with how alike: the cosine of their vectors.
type Hit = search.Hit

// Similar is the limit files most like sha, best first, of those keep accepts; not sha itself.
func (s *Service) Similar(user, sha string, limit int, keep func(sha string) bool) ([]Hit, error) {
	u := s.users[user]
	if u == nil {
		return nil, fmt.Errorf("no data_folder is configured for %s", user)
	}
	vector := u.index.Vector(sha)
	if vector == nil {
		return nil, ErrNotRead
	}
	return u.index.Search(vector, limit, func(other string) bool { return other != sha && keep(other) }), nil
}

// ErrNotRead is a file the model hasn't read (yet).
var ErrNotRead = errors.New("the file hasn't been read for similar photos yet")

// Group is files that are duplicates of each other, and the least alike pair's similarity.
type Group struct {
	Shas  []string
	Score float32
}

// Duplicates is user's groups of near-identical files, the biggest first, counting only files keep accepts (a group
// left with one is not one).
func (s *Service) Duplicates(user string, keep func(sha string) bool) ([]Group, error) {
	u := s.users[user]
	if u == nil {
		return nil, fmt.Errorf("no data_folder is configured for %s", user)
	}
	u.findDuplicates()
	u.mu.Lock()
	sets := u.groups.sets()
	u.mu.Unlock()
	var groups []Group
	for _, shas := range sets {
		shas = slices.DeleteFunc(shas, func(sha string) bool { return !keep(sha) })
		if len(shas) < 2 {
			continue
		}
		slices.Sort(shas)
		g := Group{Shas: shas, Score: 1}
		for i, a := range shas {
			va := u.index.Vector(a)
			for _, b := range shas[i+1:] {
				g.Score = min(g.Score, dot(va, u.index.Vector(b)))
			}
		}
		groups = append(groups, g)
	}
	slices.SortFunc(groups, func(a, b Group) int {
		return cmp.Or(cmp.Compare(len(b.Shas), len(a.Shas)), cmp.Compare(a.Shas[0], b.Shas[0]))
	})
	return groups, nil
}

// findDuplicates compares each vector added since the last look with every vector before it, on all the CPUs: the
// first time, the whole library (about a minute for 100,000 files), then only the new ones.
func (u *user) findDuplicates() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.index.View(func(shas []string, vectors []float32, dim int) {
		if u.checked >= len(shas) {
			return
		}
		type pair struct{ a, b int }
		found := make(chan []pair, runtime.NumCPU())
		rows := make(chan int, runtime.NumCPU())
		var wg sync.WaitGroup
		for range runtime.NumCPU() {
			wg.Go(func() {
				var mine []pair
				for i := range rows {
					vi := vectors[i*dim : (i+1)*dim]
					for j := range i {
						if dot(vi, vectors[j*dim:(j+1)*dim]) >= DuplicateSimilarity {
							mine = append(mine, pair{i, j})
						}
					}
				}
				found <- mine
			})
		}
		go func() {
			for i := u.checked; i < len(shas); i++ {
				rows <- i
			}
			close(rows)
			wg.Wait()
			close(found)
		}()
		for pairs := range found {
			for _, p := range pairs {
				u.groups.join(shas[p.a], shas[p.b])
			}
		}
		u.checked = len(shas)
	})
}

// Counts is how many of user's files have been read, of all of them; files hide reports true for are left out.
func (s *Service) Counts(user string, hide func(media.Item) bool) (read, total int) {
	u, store := s.users[user], s.library.Store(user)
	if u == nil {
		return 0, 0
	}
	store.Range(func(item media.Item) {
		if hide(item) {
			return
		}
		total++
		if u.index.Has(item.Sha256()) {
			read++
		}
	})
	return read, total
}

// Close closes the users' vector files.
func (s *Service) Close() {
	for _, u := range s.users {
		u.index.Close()
	}
}

// unionFind keeps which files are duplicates of which.
type unionFind struct{ parent map[string]string }

func newUnionFind() *unionFind { return &unionFind{parent: map[string]string{}} }

func (u *unionFind) find(a string) string {
	for {
		p, ok := u.parent[a]
		if !ok || p == a {
			return a
		}
		if gp, ok := u.parent[p]; ok {
			u.parent[a] = gp
		}
		a = p
	}
}

func (u *unionFind) join(a, b string) {
	ra, rb := u.find(a), u.find(b)
	if _, ok := u.parent[ra]; !ok {
		u.parent[ra] = ra
	}
	if _, ok := u.parent[rb]; !ok {
		u.parent[rb] = rb
	}
	if ra != rb {
		u.parent[rb] = ra
	}
}

// sets is the groups of two or more.
func (u *unionFind) sets() [][]string {
	by := map[string][]string{}
	for a := range u.parent {
		root := u.find(a)
		by[root] = append(by[root], a)
	}
	var sets [][]string
	for _, members := range by {
		if len(members) > 1 {
			sets = append(sets, members)
		}
	}
	return sets
}

func dot(a, b []float32) float32 {
	var sum float32
	for i, x := range a {
		sum += x * b[i]
	}
	return sum
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
