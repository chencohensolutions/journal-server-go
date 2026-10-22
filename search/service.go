// Package search finds photos and videos by a description in words ("a dog on the beach at sunset"), with a SigLIP
// 2 model run by ONNX Runtime on the server's CPU. The server reads each file once into a vector (the vision half of
// the model, given a 640 px preview; a video's poster frame) and keeps the vectors in the data folder's .cache; a
// search turns the text into a vector (the text half) and ranks every file by how close its vector is. See
// docs/search.md.
package search

import (
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"log"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/journal/server-go/media"
)

// How often the users' stores are looked through for files that have no vector yet.
const rescanEvery = 30 * time.Second

// Reading many files logs its progress after each this many.
const progressEvery = 1000

// Service is text search over the users' photos and videos.
type Service struct {
	model    *Model
	library  *media.Library
	previews *media.Previews
	indexes  map[string]*Index // by username
	wake     chan struct{}
}

// NewService opens the model's vectors in the data folder of each of library's users.
func NewService(model *Model, library *media.Library, previews *media.Previews) (*Service, error) {
	s := &Service{model: model, library: library, previews: previews, indexes: map[string]*Index{}, wake: make(chan struct{}, 1)}
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

// Model is the name of the model searches use.
func (s *Service) Model() string { return s.model.Name }

// Search returns the limit files of user that text describes best, best first, of those keep accepts.
func (s *Service) Search(user, text string, limit int, keep func(sha string) bool) ([]Hit, error) {
	index := s.indexes[user]
	if index == nil {
		return nil, fmt.Errorf("no data_folder is configured for %s", user)
	}
	query, err := s.model.TextVector(text)
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

// Wake has Run look for new files now rather than at its next look: after an upload.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run reads the files that have no vector yet, newest upload first, and then looks for new ones after each upload
// (Wake) and every rescanEvery (imports), until ctx ends. A file that can't be read is tried again after the
// server restarts.
func (s *Service) Run(ctx context.Context) {
	failed := map[string]bool{}
	for {
		for _, user := range s.library.Users() {
			if s.indexes[user] != nil {
				s.readNew(ctx, user, failed)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-time.After(rescanEvery):
		}
	}
}

// readNew reads user's files that have no vector yet.
func (s *Service) readNew(ctx context.Context, user string, failed map[string]bool) {
	store, index := s.library.Store(user), s.indexes[user]
	var todo []media.Item
	store.Range(func(item media.Item) {
		if !index.Has(item.Sha256()) && !failed[item.Sha256()] {
			todo = append(todo, item)
		}
	})
	if len(todo) == 0 {
		return
	}
	// Newest first: what was just uploaded can be found soonest.
	sort.Slice(todo, func(i, j int) bool { return todo[i].Sidecar.Facts.UploadedAt.After(todo[j].Sidecar.Facts.UploadedAt) })
	if len(todo) > 1 {
		log.Printf("search: reading %d files of %s with %s", len(todo), user, s.model.Name)
	}

	// Previews are made a few at a time ahead of the model, which is the slower part.
	type ready struct {
		item  media.Item
		image image.Image
		err   error
	}
	queue := make(chan media.Item)
	made := make(chan ready, 2)
	go func() {
		defer close(queue)
		for _, item := range todo {
			select {
			case queue <- item:
			case <-ctx.Done():
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			for item := range queue {
				img, err := s.preview(ctx, store, item)
				made <- ready{item, img, err}
			}
		})
	}
	go func() { wg.Wait(); close(made) }()

	var read, unreadable int
	started := time.Now()
	for r := range made {
		err := r.err
		if err == nil {
			var vector []float32
			if vector, err = s.model.ImageVector(r.image); err == nil {
				err = index.Add(r.item.Sha256(), vector)
			}
		}
		switch {
		case err == nil:
			read++
		case ctx.Err() != nil:
		default:
			failed[r.item.Sha256()] = true
			unreadable++
			// Without libvips or ffmpeg every file (or video) fails alike; the server said so at start.
			if !errors.Is(err, media.ErrPreviewsUnavailable) && !errors.Is(err, media.ErrPostersUnavailable) {
				log.Printf("search: %s: %v", r.item.Path, err)
			}
		}
		if done := read + unreadable; done%progressEvery == 0 {
			rate := float64(done) / time.Since(started).Seconds()
			left := time.Duration(float64(len(todo)-done)/rate) * time.Second
			log.Printf("search: %d of %d files of %s read (%.1f/s, about %s left)", done, len(todo), user, rate,
				left.Round(time.Minute))
		}
	}
	if len(todo) > 1 || unreadable > 0 {
		log.Printf("search: read %d files of %s in %s; %d could not be read", read, user,
			time.Since(started).Round(time.Second), unreadable)
	}
}

// preview is the picture of item that the model reads: the same 640 px one scripts/scan-nudity reads, so a kept
// preview serves both. The model squeezes it to its own square.
func (s *Service) preview(ctx context.Context, store *media.Store, item media.Item) (image.Image, error) {
	path, release, err := s.previews.Get(ctx, store, item, media.ModelPreview)
	if err != nil {
		return nil, err
	}
	defer release()
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	return img, err
}

// Close closes the users' vector files.
func (s *Service) Close() {
	for _, index := range s.indexes {
		index.Close()
	}
}
