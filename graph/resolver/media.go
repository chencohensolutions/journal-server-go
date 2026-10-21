package resolver

import (
	"context"
	"fmt"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/graph/model"
	"github.com/journal/server-go/media"
)

const (
	maxHashesPerQuery = 1000
	maxChangesPerPage = 5000
)

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

// view is what the request's device may see of its user's store: everything, unless the server keeps it from
// private files (`journal-server private <serial> withhold`).
type view struct {
	store           *media.Store
	withholdPrivate bool
}

func (r *Resolver) view(ctx context.Context) (view, error) {
	store, err := r.store(ctx)
	if err != nil {
		return view{}, err
	}
	return view{store, auth.WithholdPrivateFromContext(ctx)}, nil
}

// hides reports whether the device may not see item.
func (v view) hides(item media.Item) bool { return v.withholdPrivate && item.Sidecar.IsPrivate() }

func (v view) hidesSha(sha string) bool {
	item, ok := v.store.Get(sha)
	return ok && v.hides(item)
}

// visible leaves out of hashes the files the device may not see.
func (v view) visible(hashes []string) []string {
	if !v.withholdPrivate {
		return hashes
	}
	var shown []string
	for _, sha := range hashes {
		if !v.hidesSha(sha) {
			shown = append(shown, sha)
		}
	}
	return shown
}

func (v view) toMedia(item media.Item) *model.Media {
	versions, paired := v.store.Related(item.Sha256())
	return v.toMediaWith(item, versions, paired)
}

// toMediaWith is item as the device may see it: edits and paired files it may not see are left out.
func (v view) toMediaWith(item media.Item, versions, paired []string) *model.Media {
	m := toMediaWith(item, v.visible(versions), v.visible(paired))
	if m.VersionOf != nil && v.hidesSha(*m.VersionOf) {
		m.VersionOf = nil
	}
	if m.PairedWith != nil && v.hidesSha(*m.PairedWith) {
		m.PairedWith = nil
	}
	return m
}

func toMediaWith(item media.Item, versions, paired []string) *model.Media {
	facts := item.Sidecar.Facts
	if versions == nil {
		versions = []string{}
	}
	if paired == nil {
		paired = []string{}
	}
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
		Private:    item.Sidecar.IsPrivate(),
		URL:        "/media/" + item.Sha256(),
	}
	if facts.Kind == media.KindVideo {
		m.Kind = model.MediaKindVideo
	}
	if facts.Location != nil {
		m.Location = &model.GeoPoint{Lat: facts.Location.Lat, Lon: facts.Location.Lon}
	}
	if d := item.Sidecar.Detected; d != nil && d.Nudity != nil {
		score := d.Nudity.Score
		m.Nudity = &score
	}
	return m
}
