package media

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// The user's cameras: which camera models were theirs. They are kept in cameras.json at the data folder's root
// (beside people.json; not in settings.json, which SetHidden writes whole), written in a turn by the server only:
//
//	{ "format": 1, "cameras": [{ "make": "Canon", "model": "Canon EOS 80D", "mine": false }] }
//
// It holds only the user's own marks. A model nobody marked counts as theirs, so nothing moves until they mark one;
// the photos of a model marked "not mine" are received (SourceOf), shown as "Sent to you". See docs/media.md.

// CamerasFormat is the version of cameras.json.
const CamerasFormat = 1

// CameraMark is the user's mark on one camera model.
type CameraMark struct {
	Make  string `json:"make"`
	Model string `json:"model"`
	Mine  bool   `json:"mine"`
}

type camerasFile struct {
	Format  int          `json:"format"`
	Cameras []CameraMark `json:"cameras"`
}

// CameraMarks are the user's marks, as SourceOf reads them. The zero value marks nothing.
type CameraMarks struct {
	marks map[string]CameraMark // by Camera.key
}

// NewCameraMarks is marks read as SourceOf reads them; a later mark of the same model wins.
func NewCameraMarks(marks []CameraMark) CameraMarks {
	m := CameraMarks{marks: map[string]CameraMark{}}
	for _, mark := range marks {
		m.marks[Camera{Make: mark.Make, Model: mark.Model}.key()] = mark
	}
	return m
}

// NotMine reports whether the user marked the camera's model "not mine".
func (m CameraMarks) NotMine(c Camera) bool {
	mark, ok := m.marks[c.key()]
	return ok && !mark.Mine
}

// mark is the user's mark on the camera's model, and whether there is one.
func (m CameraMarks) mark(c Camera) (CameraMark, bool) {
	mark, ok := m.marks[c.key()]
	return mark, ok
}

// list is the marks, sorted by make and model.
func (m CameraMarks) list() []CameraMark {
	list := make([]CameraMark, 0, len(m.marks))
	for _, mark := range m.marks {
		list = append(list, mark)
	}
	sort.Slice(list, func(i, j int) bool {
		return Camera{Make: list[i].Make, Model: list[i].Model}.key() < Camera{Make: list[j].Make, Model: list[j].Model}.key()
	})
	return list
}

func (s *Store) camerasPath() string { return filepath.Join(s.root, "cameras.json") }

// readCameraMarks reads cameras.json; a data folder without one marks nothing.
func (s *Store) readCameraMarks() (CameraMarks, error) {
	data, err := os.ReadFile(s.camerasPath())
	if errors.Is(err, fs.ErrNotExist) {
		return CameraMarks{}, nil
	}
	if err != nil {
		return CameraMarks{}, err
	}
	var file camerasFile
	if err := json.Unmarshal(data, &file); err != nil {
		return CameraMarks{}, fmt.Errorf("%s: %w", s.camerasPath(), err)
	}
	if file.Format > CamerasFormat {
		return CameraMarks{}, fmt.Errorf("%s has format %d, newer than this server's %d", s.camerasPath(), file.Format, CamerasFormat)
	}
	return NewCameraMarks(file.Cameras), nil
}

// CameraMarks are the user's marks on their cameras, as read when the store opened and set since.
func (s *Store) CameraMarks() CameraMarks {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cameraMarks
}

// CameraModel is one camera model in the library: how many files it took, how many of them the import placed, the
// years they were taken in, and the user's mark. Mine is the mark, or true for a model nobody marked; Pending is how
// many of its files are still being moved after the mark changed (SetCamera).
type CameraModel struct {
	Camera
	Files, Imported int
	Years           []int
	Mine, Marked    bool
	Pending         int
}

// Cameras lists every camera model the library's files name, most files first; a model marked with no files left is
// listed too. A model is spelled as most of its files spell it.
func (s *Store) Cameras() []CameraModel {
	s.CatchUp()
	s.mu.RLock()
	defer s.mu.RUnlock()
	byKey := map[string]*CameraModel{}
	spellings := map[string]map[Camera]int{}
	years := map[string]map[int]bool{}
	for _, item := range s.items {
		facts := item.Sidecar.Facts
		if facts.Camera == nil {
			continue
		}
		key := facts.Camera.key()
		m := byKey[key]
		if m == nil {
			m = &CameraModel{}
			byKey[key] = m
			spellings[key] = map[Camera]int{}
			years[key] = map[int]bool{}
		}
		m.Files++
		if facts.Imported() {
			m.Imported++
		}
		spellings[key][*facts.Camera]++
		if facts.TakenAt != nil {
			years[key][facts.TakenAt.Year()] = true
		}
	}
	for _, mark := range s.cameraMarks.list() {
		key := Camera{Make: mark.Make, Model: mark.Model}.key()
		if byKey[key] == nil {
			byKey[key] = &CameraModel{}
			spellings[key] = map[Camera]int{{Make: mark.Make, Model: mark.Model}: 0}
		}
	}
	s.cameraMoves.mu.Lock()
	pending := s.cameraMoves.pending
	s.cameraMoves.mu.Unlock()
	models := make([]CameraModel, 0, len(byKey))
	for key, m := range byKey {
		var best Camera
		bestN := -1
		for spelling, n := range spellings[key] {
			if n > bestN || n == bestN && spelling.Make+spelling.Model < best.Make+best.Model {
				best, bestN = spelling, n
			}
		}
		m.Camera = best
		for y := range years[key] {
			m.Years = append(m.Years, y)
		}
		sort.Ints(m.Years)
		mark, marked := s.cameraMarks.mark(best)
		m.Marked, m.Mine = marked, !marked || mark.Mine
		m.Pending = pending[key]
		models = append(models, *m)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Files != models[j].Files {
			return models[i].Files > models[j].Files
		}
		return models[i].key() < models[j].key()
	})
	return models
}

// SetCamera marks a camera model as the user's or not, in cameras.json (inside a turn), and answers at once: the
// model's files then move in the background, a turn each, to received (not mine) or back to what SourceOf gives
// them (mine), and each comes again in the change feed once. Cameras says how many are still to move (Pending). A
// mark changed while a move runs is applied after it.
func (s *Store) SetCamera(maker, model string, mine bool) error {
	camera := newCamera(maker, model)
	if camera == nil {
		return errors.New("a camera needs a make or a model")
	}
	key := camera.key()
	err := s.Turn(func() error {
		s.mu.RLock()
		marks := s.cameraMarks.list()
		s.mu.RUnlock()
		kept := marks[:0]
		for _, m := range marks {
			if (Camera{Make: m.Make, Model: m.Model}).key() != key {
				kept = append(kept, m)
			}
		}
		kept = append(kept, CameraMark{Make: camera.Make, Model: camera.Model, Mine: mine})
		next := NewCameraMarks(kept)
		data, err := json.MarshalIndent(camerasFile{Format: CamerasFormat, Cameras: next.list()}, "", "  ")
		if err != nil {
			return err
		}
		if err := writeFileAtomic(s.camerasPath(), append(data, '\n')); err != nil {
			return err
		}
		s.mu.Lock()
		s.cameraMarks = next
		s.mu.Unlock()
		return nil
	})
	if err != nil {
		return err
	}
	moves := s.cameraMoving(key)
	s.cameraMoves.mu.Lock()
	if s.cameraMoves.pending == nil {
		s.cameraMoves.pending = map[string]int{}
		s.cameraMoves.queued = map[string]int{}
	}
	s.cameraMoves.queued[key]++
	if len(moves) > 0 || s.cameraMoves.pending[key] > 0 {
		s.cameraMoves.pending[key] = max(s.cameraMoves.pending[key], len(moves))
	}
	s.cameraMoves.mu.Unlock()
	s.cameraMoves.running.Add(1)
	go s.moveCamera(key)
	return nil
}

// cameraMoves are the moves SetCamera started: one runs at a time (run), and pending says how many files of each
// model are still to move.
type cameraMoves struct {
	run     sync.Mutex
	mu      sync.Mutex
	pending map[string]int // by Camera.key
	queued  map[string]int
	running sync.WaitGroup
}

// cameraMove is a file of a model whose source changes with the model's mark.
type cameraMove struct {
	sha, source string
	app         *string
}

// cameraMoving lists the files of the model key whose source changes with the marks as they are now: to received
// for a model not mine, back from received to what SourceOf gives otherwise.
func (s *Store) cameraMoving(key string) []cameraMove {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var moves []cameraMove
	for _, item := range s.items {
		facts := item.Sidecar.Facts
		if facts.Camera == nil || facts.Camera.key() != key {
			continue
		}
		current := ""
		if facts.Source != nil {
			current = *facts.Source
		}
		target, _ := SourceOf(facts, s.cameraMarks)
		switch {
		case SameSource(target, current):
		case target == SourceReceived, SameSource(current, SourceReceived):
			moves = append(moves, cameraMove{sha: item.Sha256(), source: target, app: facts.ScreenshotApp})
		}
	}
	return moves
}

// moveCamera moves the files of the model key as its mark now says, after any move before it.
func (s *Store) moveCamera(key string) {
	defer s.cameraMoves.running.Done()
	s.cameraMoves.run.Lock()
	defer s.cameraMoves.run.Unlock()
	moves := s.cameraMoving(key)
	setPending := func(n int) {
		s.cameraMoves.mu.Lock()
		defer s.cameraMoves.mu.Unlock()
		if n > 0 || s.cameraMoves.queued[key] > 1 {
			s.cameraMoves.pending[key] = n
		} else {
			delete(s.cameraMoves.pending, key)
		}
	}
	setPending(len(moves))
	for i, m := range moves {
		if _, err := s.Relabel(m.sha, m.source, m.app); err != nil {
			log.Printf("media: moving %s to %s after a camera's mark changed: %v", m.sha, m.source, err)
		}
		setPending(len(moves) - i - 1)
	}
	s.cameraMoves.mu.Lock()
	s.cameraMoves.queued[key]--
	if s.cameraMoves.queued[key] <= 0 {
		delete(s.cameraMoves.queued, key)
		delete(s.cameraMoves.pending, key)
	}
	s.cameraMoves.mu.Unlock()
}

// WaitCameraMoves waits until the moves SetCamera started are done.
func (s *Store) WaitCameraMoves() { s.cameraMoves.running.Wait() }

// Relabel sets a file's source and the app of a screenshot, in one rewrite of its sidecar inside one turn: SentFrom
// keeps the source it arrived with (keepSentFrom), and CorrectedAt is set, so the file comes again in the change
// feed once. Notes, nudity and privacy stay as they are; the original is never touched. A file that already has both
// is left as it is.
func (s *Store) Relabel(sha, source string, screenshotApp *string) (changed bool, err error) {
	source = NormalizeSource(source)
	err = s.Turn(func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		item, ok := s.items[sha]
		if !ok {
			return fmt.Errorf("%s isn't in the data folder", sha)
		}
		facts := item.Sidecar.Facts
		if old := facts.Source; old != nil && *old == source && equalString(facts.ScreenshotApp, screenshotApp) {
			return nil
		}
		sidecar := item.Sidecar
		now := s.now().UTC()
		keepSentFrom(&sidecar.Facts)
		sidecar.Facts.Source = &source
		sidecar.Facts.ScreenshotApp = screenshotApp
		sidecar.Facts.CorrectedAt = &now
		changed = true
		return s.rewriteSidecar(item, sidecar)
	})
	return changed, err
}
