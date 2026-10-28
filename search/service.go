// Package search finds photos and videos by a description in words ("a dog on the beach at sunset"), with a SigLIP
// 2 model run by ONNX Runtime on the server's CPU. The server reads each file once into a vector (the vision half of
// the model, given the original squeezed to the model's square; a video's poster frame; package reading does the
// reading) and keeps the vectors in the data folder's .cache; a
// search turns the text into a vector (the text half) and ranks every file by how close its vector is. See
// docs/search.md.
package search

import (
	"context"
	"fmt"

	"github.com/journal/server-go/media"
	"github.com/journal/server-go/reading"
)

// Service is text search over the users' photos and videos. It is an index of package reading, which reads each
// file once for every index and calls Read.
type Service struct {
	model *Model
	// The text half loaded only while searches come (NewOnDemandService); nil when model is loaded for good.
	onDemand *onDemand
	library  *media.Library
	indexes  map[string]*Index // by username
}

// NewService opens the model's vectors in the data folder of each of library's users.
func NewService(model *Model, library *media.Library) (*Service, error) {
	s := &Service{model: model, library: library, indexes: map[string]*Index{}}
	for _, user := range library.Users() {
		store := library.Store(user)
		index, err := OpenIndex(store.Root(), model.Name, model.Dim)
		if err != nil {
			return nil, fmt.Errorf("user %s: %w", user, err)
		}
		s.indexes[user] = index
	}
	return s, nil
}

// Refresh reads the vectors another process added to the users' files since the last look (NewOnDemandService).
func (s *Service) Refresh() error {
	for user, index := range s.indexes {
		if _, err := index.Refresh(); err != nil {
			return fmt.Errorf("user %s: %w", user, err)
		}
	}
	return nil
}

// Prepare gets the model ready for a search that is about to come (an app opening its search screen): a server that
// loads the text half only to search starts loading it now, in the background. It returns the state at once.
func (s *Service) Prepare() ModelState {
	if s.onDemand == nil {
		return Ready
	}
	return s.onDemand.prepare()
}

// State is whether a search's words can be read at once.
func (s *Service) State() ModelState {
	if s.onDemand == nil {
		return Ready
	}
	return s.onDemand.state()
}

// WaitWhileLoading waits for a load in progress (Prepare, or a search's) to end, or for ctx to end, and returns the
// state then: an app waits for the model this way rather than asking again and again.
func (s *Service) WaitWhileLoading(ctx context.Context) ModelState {
	if s.onDemand == nil {
		return Ready
	}
	return s.onDemand.wait(ctx)
}

// Model is the name of the model searches use.
func (s *Service) Model() string { return s.model.Name }

// Search returns the limit files of user that text describes best, best first, of those keep accepts.
func (s *Service) Search(user, text string, limit int, keep func(sha string) bool) ([]Hit, error) {
	index := s.indexes[user]
	if index == nil {
		return nil, fmt.Errorf("no data_folder is configured for %s", user)
	}
	var query []float32
	var err error
	if s.onDemand != nil {
		query, err = s.onDemand.vector(text)
	} else {
		query, err = s.model.TextVector(text)
	}
	if err != nil {
		return nil, err
	}
	return index.Search(query, limit, keep), nil
}

// Vector is a copy of sha's vector in user's index, or nil when it hasn't been read.
func (s *Service) Vector(user, sha string) []float32 {
	if index := s.indexes[user]; index != nil {
		return index.Vector(sha)
	}
	return nil
}

// CacheFile is user's vectors file; "" for a user with no data folder.
func (s *Service) CacheFile(user string) string {
	if index := s.indexes[user]; index != nil {
		return index.Path()
	}
	return ""
}

// Has reports whether user's file sha has a vector.
func (s *Service) Has(user, sha string) bool {
	index := s.indexes[user]
	return index != nil && index.Has(sha)
}

// Record is user's file sha's record as the vectors file keeps it; nil when it has no vector.
func (s *Service) Record(user, sha string) []byte {
	if index := s.indexes[user]; index != nil {
		return index.Record(sha)
	}
	return nil
}

// Counts is how many of user's files have been read, and so can be found, of all of them; files hide reports true
// for are left out of both.
func (s *Service) Counts(user string, hide func(media.Item) bool) (indexed, total int) {
	index, store := s.indexes[user], s.library.Store(user)
	if index == nil {
		return 0, 0
	}
	store.Range(func(item media.Item) {
		if hide(item) {
			return
		}
		total++
		if index.Has(item.Sha256()) {
			indexed++
		}
	})
	return indexed, total
}

// Picture is what the model reads of a file: the original squeezed to the model's square, as SigLIP was trained.
func (s *Service) Picture() media.PreviewSpec {
	return media.PreviewSpec{Width: s.model.size, Height: s.model.size, Fit: media.SqueezeFit}
}

func (s *Service) Name() string { return "search" }

// Order: after the nudity scan and faces; finding by words matters least soon.
func (s *Service) Order() int { return 20 }

func (s *Service) Wants() reading.Wants {
	return reading.Wants{Kinds: []media.Kind{media.KindPhoto, media.KindVideo}, Pictures: []media.PreviewSpec{s.Picture()}}
}

// Needs reports whether item has no vector yet.
func (s *Service) Needs(user string, item media.Item) bool {
	index := s.indexes[user]
	return index != nil && !index.Has(item.Sha256())
}

// Read reads the file into its vector.
func (s *Service) Read(ctx context.Context, user string, in reading.Input) error {
	vector, err := s.model.ImageVector(in.Pictures[s.Picture()])
	if err != nil {
		return err
	}
	return s.indexes[user].Add(in.Item.Sha256(), vector)
}

// Carry gives the file to the vector of the file from, whose pictures are the same (a JPEG kept as JPEG XL), so it
// isn't read again; false when from has none.
func (s *Service) Carry(user, from, to string) (bool, error) {
	index := s.indexes[user]
	if index == nil {
		return false, nil
	}
	vector := index.Vector(from)
	if vector == nil {
		return false, nil
	}
	return true, index.Add(to, vector)
}

func (s *Service) Finish(user string) {}

func (s *Service) FailureLog() (folder, name string) { return "search", s.model.Name + ".failed.log" }

// Close closes the users' vector files.
func (s *Service) Close() {
	for _, index := range s.indexes {
		index.Close()
	}
	if o := s.onDemand; o != nil {
		o.mu.Lock()
		if o.timer != nil {
			o.timer.Stop()
		}
		if o.model != nil {
			o.model.Close()
			o.model = nil
		}
		o.mu.Unlock()
	}
}
