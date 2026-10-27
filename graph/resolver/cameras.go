package resolver

import (
	"context"

	"github.com/journal/server-go/graph/model"
	"github.com/journal/server-go/media"
)

// cameras lists the user's camera models (all their files, hidden or not, like hiddenCollections).
func (r *Resolver) cameras(ctx context.Context) ([]*model.CameraModel, error) {
	store, err := r.store(ctx)
	if err != nil {
		return nil, err
	}
	return toCameraModels(store.Cameras()), nil
}

// setCamera marks a model and answers the list at once; its files move in the background (Store.SetCamera).
func (r *Resolver) setCamera(ctx context.Context, maker, cameraModel string, mine bool) ([]*model.CameraModel, error) {
	store, err := r.store(ctx)
	if err != nil {
		return nil, err
	}
	if err := store.SetCamera(maker, cameraModel, mine); err != nil {
		return nil, err
	}
	return toCameraModels(store.Cameras()), nil
}

func toCameraModels(models []media.CameraModel) []*model.CameraModel {
	out := make([]*model.CameraModel, len(models))
	for i, m := range models {
		years := m.Years
		if years == nil {
			years = []int{}
		}
		out[i] = &model.CameraModel{Make: m.Make, Model: m.Model, Files: m.Files, Imported: m.Imported, Years: years,
			Mine: m.Mine, Marked: m.Marked, Pending: m.Pending}
	}
	return out
}
