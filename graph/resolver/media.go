package resolver

import (
	"context"
	"fmt"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/graph/model"
	"github.com/journal/server-go/media"
)

const maxHashesPerQuery = 1000

// store is the data folder of the request's user.
func (r *Resolver) store(ctx context.Context) (*media.Store, error) {
	user := auth.FromContext(ctx)
	if user == nil {
		return nil, fmt.Errorf("unauthorized")
	}
	store := r.Media.Store(user.Username)
	if store == nil {
		return nil, fmt.Errorf("no data_folder is configured for %s", user.Username)
	}
	return store, nil
}

func toMedia(store *media.Store, item media.Item) *model.Media {
	facts := item.Sidecar.Facts
	versions, paired := store.Related(item.Sha256())
	m := &model.Media{
		Sha256:     item.Sha256(),
		Kind:       model.MediaKindPhoto,
		FileName:   facts.FileName,
		MimeType:   facts.MimeType,
		Size:       facts.Size,
		TakenAt:    facts.TakenAt,
		Width:      facts.Width,
		Height:     facts.Height,
		DurationMs: facts.DurationMs,
		Source:     facts.Source,
		VersionOf:  facts.VersionOf,
		Versions:   versions,
		PairedWith: facts.PairedWith,
		Paired:     paired,
		UploadedAt: facts.UploadedAt,
		UploadedBy: facts.UploadedBy,
		URL:        "/media/" + item.Sha256(),
	}
	if facts.Kind == media.KindVideo {
		m.Kind = model.MediaKindVideo
	}
	if facts.Location != nil {
		m.Location = &model.GeoPoint{Lat: facts.Location.Lat, Lon: facts.Location.Lon}
	}
	return m
}
