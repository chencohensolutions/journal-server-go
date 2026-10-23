package search

import (
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/journal/server-go/media"
	"github.com/journal/server-go/onnx"
)

// IdleRelease is how long the text half of the model stays loaded after the last search, in a server that loads
// it only to search (NewOnDemandService): 1.15 GB of memory, against about 4 s to load it again.
var IdleRelease = 10 * time.Minute

// ModelState is whether a search's words can be read at once (prepareSearch, searchModel).
type ModelState int

const (
	// Ready: the text half is loaded, a search answers at once.
	Ready ModelState = iota
	// Loading: the text half is being loaded, about 4 s; a search now waits for it.
	Loading
	// Unloaded: the text half isn't loaded; the next search, or Prepare, loads it.
	Unloaded
)

// onDemand is the model's text half, loaded at the first search (or Prepare) and released after IdleRelease without
// one.
type onDemand struct {
	rt      *onnx.Runtime
	folder  string
	threads int
	dim     func() int // the vectors' length, to check the model against; 0 when not known yet

	mu       sync.Mutex
	model    *Model
	loading  chan struct{} // closed when the load in progress ends; nil when none is
	loadErr  error         // why the last load failed
	inUse    int
	lastUsed time.Time
	timer    *time.Timer
}

// NewOnDemandService searches the vectors another process writes (journal-server index), each user's opened to
// read only (Refresh reads what that process added), and loads the model's text half (1.15 GB for SigLIP 2 base) only when a search comes, and
// releases it after IdleRelease without searches: the first search after a quiet spell waits for it, about 4 s.
func NewOnDemandService(rt *onnx.Runtime, folder string, threads int, library *media.Library) (*Service, error) {
	abs, err := filepath.Abs(folder)
	if err != nil {
		return nil, err
	}
	s := &Service{model: &Model{Name: filepath.Base(abs)}, library: library, indexes: map[string]*Index{}}
	for _, user := range library.Users() {
		index, err := OpenVectorsReadOnly(filepath.Join(library.Store(user).Root(), ".cache", "search", s.model.Name+".vectors"), 0)
		if err != nil {
			return nil, fmt.Errorf("user %s: %w", user, err)
		}
		s.indexes[user] = index
	}
	s.onDemand = &onDemand{rt: rt, folder: folder, threads: threads, dim: func() int {
		for _, index := range s.indexes {
			if d := index.Dim(); d != 0 {
				return d
			}
		}
		return 0
	}}
	return s, nil
}

// vector is text's vector, loading the text half first if it isn't loaded.
func (o *onDemand) vector(text string) ([]float32, error) {
	o.mu.Lock()
	for o.model == nil {
		if o.loading == nil {
			o.startLoad("a search")
		}
		loading := o.loading
		o.mu.Unlock()
		<-loading
		o.mu.Lock()
		if o.model == nil && o.loading == nil {
			err := o.loadErr
			o.mu.Unlock()
			return nil, err
		}
	}
	o.inUse++
	m := o.model
	o.mu.Unlock()

	vector, err := m.TextVector(text)

	o.mu.Lock()
	o.inUse--
	o.used()
	o.mu.Unlock()
	return vector, err
}

// prepare starts loading the text half if it isn't loaded or loading, so the search that follows (an app opening
// its search screen) doesn't wait, and keeps it loaded IdleRelease longer. It returns the state at once.
func (o *onDemand) prepare() ModelState {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case o.model != nil:
		o.used()
		return Ready
	case o.loading == nil:
		o.startLoad("an app about to search")
	}
	return Loading
}

// state is whether the text half is loaded, being loaded, or neither.
func (o *onDemand) state() ModelState {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case o.model != nil:
		return Ready
	case o.loading != nil:
		return Loading
	}
	return Unloaded
}

// startLoad loads the text half in the background; o.mu is held. Whoever waits on o.loading finds o.model set, or
// o.loadErr.
func (o *onDemand) startLoad(why string) {
	loading := make(chan struct{})
	o.loading = loading
	go func() {
		started := time.Now()
		m, err := LoadTextModel(o.rt, o.folder, o.threads)
		if err == nil {
			if d := o.dim(); d != 0 && d != m.Dim {
				err = fmt.Errorf("%s makes vectors of %d numbers, the photos' have %d", m.Name, m.Dim, d)
				m.Close()
				m = nil
			}
		}
		o.mu.Lock()
		defer o.mu.Unlock()
		o.model, o.loadErr, o.loading = m, err, nil
		close(loading)
		if err != nil {
			log.Printf("search: the text half could not be loaded for %s: %v", why, err)
			return
		}
		o.used()
		log.Printf("search: %s's text half loaded in %s for %s; released after %s without a search", m.Name,
			time.Since(started).Round(time.Millisecond), why, IdleRelease)
	}()
}

// used notes a use now, so the text half is kept IdleRelease longer; o.mu is held.
func (o *onDemand) used() {
	o.lastUsed = time.Now()
	if o.timer == nil {
		o.timer = time.AfterFunc(IdleRelease, o.releaseIfIdle)
	} else {
		o.timer.Reset(IdleRelease)
	}
}

// releaseIfIdle releases the text half when no search has come for IdleRelease.
func (o *onDemand) releaseIfIdle() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.model == nil || o.inUse > 0 || time.Since(o.lastUsed) < IdleRelease {
		return
	}
	o.model.Close()
	o.model = nil
	log.Printf("search: the text half released after %s without a search", IdleRelease)
}

// loaded reports whether the text half is loaded now.
func (o *onDemand) loaded() bool { return o.state() == Ready }
