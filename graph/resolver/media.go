package resolver

import (
	"context"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/graph/model"
	"github.com/journal/server-go/media"
)

const (
	maxHashesPerQuery = 1000
	maxChangesPerPage = 5000
	maxSearchHits     = 1000
	maxSearchDepth    = 10000 // offset + limit: how far down the matches a page may reach
	maxSearchText     = 1000  // bytes; the model reads 64 tokens at most anyway
)

// store is the data folder of the request's user.
func (r *Resolver) store(ctx context.Context) (*media.Store, error) {
	user := auth.FromContext(ctx)
	if user == nil {
		return nil, coded(CodeUnauthorized, "client certificate required")
	}
	store := r.Library.Store(user.Username)
	if store == nil {
		return nil, coded(CodeNoDataFolder, "no data_folder is configured for %s", user.Username)
	}
	return store, nil
}

// view is what the request's device may see of its user's store (media.View): everything but the user's hidden
// collections, and without private files when the server keeps them from it
// (`journal-server private <serial> withhold`).
type view struct {
	store *media.Store
	media.View
}

func (r *Resolver) view(ctx context.Context) (view, error) {
	store, err := r.store(ctx)
	if err != nil {
		return view{}, err
	}
	return view{store, store.ViewFor(auth.WithholdPrivateFromContext(ctx))}, nil
}

// hides reports whether the device may not see item.
func (v view) hides(item media.Item) bool { return v.store.Hides(v.View, item) }

func (v view) hidesSha(sha string) bool {
	item, ok := v.store.Get(sha)
	return ok && v.hides(item)
}

// visible leaves out of hashes the files the device may not see.
func (v view) visible(hashes []string) []string {
	if !v.WithholdPrivate && v.Hidden.IsEmpty() {
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

// changePage is a page of the feed as the device may see it: files it may not see come as deleted, and as hidden
// when the user hides them (the device leaves out its own copy too).
func (v view) changePage(items []media.Item, next string, remaining int) *model.ChangePage {
	versions, paired := v.store.RelatedIndex()
	changes := make([]*model.Change, len(items))
	for i, item := range items {
		changes[i] = &model.Change{Kind: model.ChangeKindMedia, Path: "media/" + item.Sha256()}
		if v.hides(item) {
			changes[i].Deleted = true
			changes[i].Hidden = v.store.Hides(media.View{Hidden: v.Hidden}, item)
		} else {
			changes[i].Media = v.toMediaWith(item, versions[item.Sha256()], paired[item.Sha256()])
		}
	}
	return &model.ChangePage{Changes: changes, Cursor: next, HasMore: remaining > 0, Remaining: remaining}
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
		App:        facts.App,
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
