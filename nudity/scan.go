package nudity

import (
	"context"
	"errors"
	"image"
	_ "image/jpeg"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/journal/server-go/media"
)

// How often the users' stores are looked through for files not scanned yet (imports; uploads wake the scan).
const rescanEvery = 30 * time.Second

// Scanning many files logs its progress after each this many.
const progressEvery = 1000

// ToScan is store's files not scanned by this model yet (all of them with rescan), newest upload first: what was
// just uploaded is hidden soonest. Files skip reports true for are left out.
func ToScan(store *media.Store, rescan bool, skip func(media.Item) bool) []media.Item {
	var todo []media.Item
	store.Range(func(item media.Item) {
		if skip != nil && skip(item) {
			return
		}
		if d := item.Sidecar.Detected; rescan || d == nil || d.Nudity == nil || d.Nudity.Model != ModelName {
			todo = append(todo, item)
		}
	})
	sort.Slice(todo, func(i, j int) bool { return todo[i].Sidecar.Facts.UploadedAt.After(todo[j].Sidecar.Facts.UploadedAt) })
	return todo
}

// Scan scores todo with m and writes each answer into the file's sidecar (Store.SetNudity, in a turn, so the
// server, scripts and devices learn of it). Previews are made jobs at a time ahead of the model. done is told of
// each file, with the error that kept it from being scanned; it stops when ctx ends.
func Scan(ctx context.Context, m *Model, store *media.Store, previews *media.Previews, todo []media.Item, jobs int,
	done func(media.Item, Result, error)) {
	type ready struct {
		item  media.Item
		image image.Image
		err   error
	}
	queue := make(chan media.Item)
	made := make(chan ready, jobs)
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
	for range jobs {
		wg.Go(func() {
			for item := range queue {
				img, err := preview(ctx, store, previews, item)
				made <- ready{item, img, err}
			}
		})
	}
	go func() { wg.Wait(); close(made) }()
	for r := range made {
		if ctx.Err() != nil {
			continue
		}
		var result Result
		err := r.err
		if err == nil {
			if result, err = m.Score(r.image); err == nil {
				err = store.SetNudity(r.item.Sha256(), media.Detection{Score: result.Score, Model: ModelName})
			}
		}
		done(r.item, result, err)
	}
}

// preview is the picture of item the model reads: the 640 px one search reads too (a video's poster frame).
func preview(ctx context.Context, store *media.Store, previews *media.Previews, item media.Item) (image.Image, error) {
	path, release, err := previews.Get(ctx, store, item, media.ModelPreview)
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

// Service scans the users' new files as they come, in the server.
type Service struct {
	model    *Model
	library  *media.Library
	previews *media.Previews
	wake     chan struct{}
}

// NewService scans the files of library's users with model.
func NewService(model *Model, library *media.Library, previews *media.Previews) *Service {
	return &Service{model: model, library: library, previews: previews, wake: make(chan struct{}, 1)}
}

// Wake has Run look for new files now rather than at its next look: after an upload.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run scans the files not scanned yet, newest upload first, and then looks for new ones after each upload (Wake)
// and every rescanEvery (imports), until ctx ends. A file that can't be scanned is tried again after the server
// restarts.
func (s *Service) Run(ctx context.Context) {
	failed := map[string]bool{}
	for {
		for _, user := range s.library.Users() {
			s.scanNew(ctx, user, failed)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-time.After(rescanEvery):
		}
	}
}

func (s *Service) scanNew(ctx context.Context, user string, failed map[string]bool) {
	store := s.library.Store(user)
	todo := ToScan(store, false, func(item media.Item) bool { return failed[item.Sha256()] })
	if len(todo) == 0 {
		return
	}
	if len(todo) > 1 {
		log.Printf("nudity: scanning %d files of %s with %s", len(todo), user, ModelName)
	}
	var scanned, unreadable, flagged int
	started := time.Now()
	failures := media.NewFailureLog(store, "nudity", "nudity", ModelName+".failed.log")
	defer failures.Close()
	if err := os.MkdirAll(filepath.Dir(failures.Path), 0o755); err != nil {
		log.Printf("nudity: %v", err)
	}
	Scan(ctx, s.model, store, s.previews, todo, 2, func(item media.Item, result Result, err error) {
		switch {
		case err == nil:
			scanned++
			if result.Score >= media.DefaultNudityFrom {
				flagged++
			}
		case ctx.Err() != nil:
		default:
			failed[item.Sha256()] = true
			unreadable++
			failures.Add(item, err)
			// Without libvips or ffmpeg every file (or video) fails alike; the server said so at start.
			if !errors.Is(err, media.ErrPreviewsUnavailable) && !errors.Is(err, media.ErrPostersUnavailable) {
				log.Printf("nudity: %s: %v", item.Path, err)
			}
		}
		if done := scanned + unreadable; done%progressEvery == 0 {
			rate := float64(done) / time.Since(started).Seconds()
			left := time.Duration(float64(len(todo)-done)/rate) * time.Second
			log.Printf("nudity: %d of %d files of %s scanned (%.1f/s, about %s left)", done, len(todo), user, rate,
				left.Round(time.Minute))
		}
	})
	if len(todo) > 1 || unreadable > 0 {
		log.Printf("nudity: scanned %d files of %s in %s, %d scored %.1f or more; %d could not be scanned", scanned, user,
			time.Since(started).Round(time.Second), flagged, media.DefaultNudityFrom, unreadable)
	}
	if unreadable > 0 {
		log.Printf("nudity: the files that could not be scanned are listed in %s", failures.Path)
	}
}
