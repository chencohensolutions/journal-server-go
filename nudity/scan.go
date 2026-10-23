package nudity

import (
	"context"
	"image"
	_ "image/jpeg"
	"sort"
	"sync"

	"github.com/journal/server-go/media"
	"github.com/journal/server-go/reading"
)

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

// Picture is what the model reads of a file, made from its original (a video's poster frame): the photo fitted in
// 320×320, the model's own square (Model.Score pads it with black, as NudeNet does).
var Picture = media.PreviewSpec{Width: side, Height: side, Fit: "contain"}

// preview is the picture of item the model reads, decoded from the original.
func preview(ctx context.Context, store *media.Store, previews *media.Previews, item media.Item) (image.Image, error) {
	o, err := previews.Open(ctx, store, item, side)
	if err != nil {
		return nil, err
	}
	defer o.Close()
	return o.Picture(Picture)
}

// Service scans the users' new files as they come, in the server: an index of package reading, which reads each
// file once for every index and calls Read.
type Service struct {
	model *Model
}

// NewService scans with model.
func NewService(model *Model) *Service { return &Service{model: model} }

func (s *Service) Name() string { return "nudity" }

// Order: first, so that a new photo is hidden before it can be found.
func (s *Service) Order() int { return 0 }

func (s *Service) Wants() reading.Wants {
	return reading.Wants{Kinds: []media.Kind{media.KindPhoto, media.KindVideo}, Pictures: []media.PreviewSpec{Picture}}
}

// Needs reports whether item hasn't been scanned by this model.
func (s *Service) Needs(user string, item media.Item) bool {
	d := item.Sidecar.Detected
	return d == nil || d.Nudity == nil || d.Nudity.Model != ModelName
}

// Read scores the file and writes the answer into its sidecar (Store.SetNudity, in a turn).
func (s *Service) Read(ctx context.Context, user string, in reading.Input) error {
	result, err := s.model.Score(in.Pictures[Picture])
	if err != nil {
		return err
	}
	return in.Store.SetNudity(in.Item.Sha256(), media.Detection{Score: result.Score, Model: ModelName})
}

func (s *Service) Finish(user string) {}

func (s *Service) FailureLog() (folder, name string) { return "nudity", ModelName + ".failed.log" }
