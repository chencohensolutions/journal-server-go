package resolver

import (
	"github.com/journal/server-go/graph/model"
	"github.com/journal/server-go/media"
)

func hiddenCollections(store *media.Store) *model.HiddenCollections {
	whatsapp, withoutApp, apps := store.Collections()
	result := &model.HiddenCollections{
		Whatsapp:              toCollection(whatsapp),
		ScreenshotsWithoutApp: toCollection(withoutApp),
		ScreenshotApps:        make([]*model.Collection, len(apps)),
	}
	for i, app := range apps {
		result.ScreenshotApps[i] = toCollection(app)
	}
	return result
}

func toCollection(c media.Collection) *model.Collection {
	return &model.Collection{Name: c.Name, Files: c.Files, Hidden: c.Hidden}
}
