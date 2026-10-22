// Package faces finds photos by the people in them, with two small open models run by ONNX Runtime on the server's
// CPU: YuNet finds the faces in a photo, SFace turns each into a vector, and faces of the same person point the same
// way. The server reads each file once (a 1280 px preview; a video's poster frame) and keeps its faces in the data
// folder's .cache. Faces alike are put in groups; the user names a group once ("Mom"), and from then on her faces
// are found as they come. The names, and the faces the user moved, are kept in people.json at the data folder's
// root. See docs/faces.md.
package faces

import (
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"log"
	"os"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/journal/server-go/media"
)

// How often the users' stores are looked through for files not read yet.
const rescanEvery = 30 * time.Second

// Reading many files logs its progress after each this many.
const progressEvery = 1000

// Faces narrower than this many pixels in the preview are not kept: nothing can be told from them.
const minPixels = 16

// Service is face search over the users' photos and videos.
type Service struct {
	model    *Model
	library  *media.Library
	previews *media.Previews
	users    map[string]*people // by username
	wake     chan struct{}
}

// NewService opens the model's faces and the people named in the data folder of each of library's users.
func NewService(model *Model, library *media.Library, previews *media.Previews) (*Service, error) {
	s := &Service{model: model, library: library, previews: previews, users: map[string]*people{}, wake: make(chan struct{}, 1)}
	for _, user := range library.Users() {
		store := library.Store(user)
		index, err := OpenIndex(store.Root(), model.Name)
		if err != nil {
			return nil, fmt.Errorf("user %s: %w", user, err)
		}
		if s.users[user], err = newPeople(store, index); err != nil {
			return nil, fmt.Errorf("user %s: %w", user, err)
		}
	}
	return s, nil
}

// Model is the name of the models the faces were found with.
func (s *Service) Model() string { return s.model.Name }

func (s *Service) user(user string) (*people, error) {
	p := s.users[user]
	if p == nil {
		return nil, fmt.Errorf("no data_folder is configured for %s", user)
	}
	return p, nil
}

// People is user's named people and (withGroups) groups of alike faces, as a device sees them: only faces in files
// keep accepts count, and those with none are left out.
func (s *Service) People(user string, keep func(sha string) bool, withGroups bool) ([]Person, error) {
	p, err := s.user(user)
	if err != nil {
		return nil, err
	}
	return p.list(keep, withGroups), nil
}

// FacesIn is the faces found in user's file sha, biggest first, and whom each is.
func (s *Service) FacesIn(user, sha string) []FaceInfo {
	if p := s.users[user]; p != nil {
		return p.facesIn(sha)
	}
	return nil
}

// Files is user's files that have a face of every one of people (IDs of people or groups).
func (s *Service) Files(user string, people []string) (map[string]bool, error) {
	p, err := s.user(user)
	if err != nil {
		return nil, err
	}
	return p.files(people)
}

// Similar is the limit files of user, of those keep accepts, with a face most like face, best first.
func (s *Service) Similar(user string, face FaceID, limit int, keep func(sha string) bool) ([]FaceHit, error) {
	p, err := s.user(user)
	if err != nil {
		return nil, err
	}
	return p.similar(face, limit, keep)
}

// Name names a group, or renames a person, and returns the person's ID; see people.name.
func (s *Service) Name(user, id, name string) (string, error) {
	p, err := s.user(user)
	if err != nil {
		return "", err
	}
	return p.name(id, name)
}

// Merge puts the faces of from (people or groups) in the person into.
func (s *Service) Merge(user, into string, from []string) error {
	p, err := s.user(user)
	if err != nil {
		return err
	}
	return p.merge(into, from)
}

// Move gives face to the person to ("": no one), and keeps it from the person it was.
func (s *Service) Move(user string, face FaceID, to string) error {
	p, err := s.user(user)
	if err != nil {
		return err
	}
	return p.move(face, to)
}

// Forget removes a person; their faces are put in groups again.
func (s *Service) Forget(user, id string) error {
	p, err := s.user(user)
	if err != nil {
		return err
	}
	return p.forget(id)
}

// Counts is how many of user's files have been read for faces, of all of them; files hide reports true for are left
// out of both.
func (s *Service) Counts(user string, hide func(media.Item) bool) (read, total int) {
	p, store := s.users[user], s.library.Store(user)
	if p == nil {
		return 0, 0
	}
	store.Range(func(item media.Item) {
		if hide(item) {
			return
		}
		total++
		if p.index.Has(item.Sha256()) {
			read++
		}
	})
	return read, total
}

// Wake has Run look for new files now rather than at its next look: after an upload.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run reads the files not read yet, newest upload first, and then looks for new ones after each upload (Wake) and
// every rescanEvery (imports), until ctx ends. A file that can't be read is tried again after the server restarts.
func (s *Service) Run(ctx context.Context) {
	failed := map[string]bool{}
	for {
		for _, user := range s.library.Users() {
			if p := s.users[user]; p != nil {
				s.readNew(ctx, user, p, failed)
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

// readNew reads user's files not read yet, then puts their faces with the others.
func (s *Service) readNew(ctx context.Context, user string, p *people, failed map[string]bool) {
	store := s.library.Store(user)
	var todo []media.Item
	store.Range(func(item media.Item) {
		if !p.index.Has(item.Sha256()) && !failed[item.Sha256()] {
			todo = append(todo, item)
		}
	})
	if len(todo) == 0 {
		return
	}
	// Newest first: what was just uploaded can be found soonest.
	sort.Slice(todo, func(i, j int) bool { return todo[i].Sidecar.Facts.UploadedAt.After(todo[j].Sidecar.Facts.UploadedAt) })
	if len(todo) > 1 {
		log.Printf("faces: reading %d files of %s with %s", len(todo), user, s.model.Name)
	}

	// Previews are made a few at a time ahead of the models, which are the slower part.
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

	var read, unreadable, found int
	started := time.Now()
	failures := media.NewFailureLog(store, "faces", "faces", s.model.Name+".failed.log")
	defer failures.Close()
	for r := range made {
		err := r.err
		if err == nil {
			var faces []Face
			if faces, err = s.model.Faces(r.image); err == nil {
				faces = keepable(faces, r.image.Bounds().Dx())
				found += len(faces)
				err = p.index.Add(r.item.Sha256(), faces)
			}
		}
		switch {
		case err == nil:
			read++
		case ctx.Err() != nil:
		default:
			failed[r.item.Sha256()] = true
			unreadable++
			failures.Add(r.item, err)
			// Without libvips or ffmpeg every file (or video) fails alike; the server said so at start.
			if !errors.Is(err, media.ErrPreviewsUnavailable) && !errors.Is(err, media.ErrPostersUnavailable) {
				log.Printf("faces: %s: %v", r.item.Path, err)
			}
		}
		if done := read + unreadable; done%progressEvery == 0 {
			rate := float64(done) / time.Since(started).Seconds()
			left := time.Duration(float64(len(todo)-done)/rate) * time.Second
			log.Printf("faces: %d of %d files of %s read (%.1f/s, about %s left)", done, len(todo), user, rate,
				left.Round(time.Minute))
		}
	}
	p.mu.Lock()
	p.update()
	p.mu.Unlock()
	if len(todo) > 1 || unreadable > 0 {
		log.Printf("faces: read %d files of %s in %s, %d faces found; %d could not be read", read, user,
			time.Since(started).Round(time.Second), found, unreadable)
	}
	if unreadable > 0 {
		log.Printf("faces: the files that could not be read are listed in %s", failures.Path)
	}
}

// keepable is faces without those too small to tell apart, biggest first.
func keepable(faces []Face, width int) []Face {
	faces = slices.DeleteFunc(faces, func(f Face) bool { return f.Box[2]*float32(width) < minPixels })
	slices.SortStableFunc(faces, func(a, b Face) int {
		areaA, areaB := a.Box[2]*a.Box[3], b.Box[2]*b.Box[3]
		switch {
		case areaA > areaB:
			return -1
		case areaA < areaB:
			return 1
		}
		return 0
	})
	return faces
}

// preview is the picture of item the models read: 1280 px, so faces at the back of a group are still clear.
func (s *Service) preview(ctx context.Context, store *media.Store, item media.Item) (image.Image, error) {
	path, release, err := s.previews.Get(ctx, store, item, media.FacePreview)
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

// Close closes the users' face files.
func (s *Service) Close() {
	for _, p := range s.users {
		p.index.Close()
	}
}
