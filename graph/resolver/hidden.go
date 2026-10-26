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
	hidden := store.Hidden()
	nudity := &model.NudityHiding{Hidden: hidden.NudityFrom > 0, From: hidden.NudityFrom}
	if !nudity.Hidden {
		nudity.From = media.DefaultNudityFrom
	}
	nudity.Files, nudity.NotScanned = store.NudityCounts(nudity.From)
	result.Nudity = nudity
	recordings, calls, voiceMessages := store.AudioCollections()
	result.Recordings, result.Calls, result.VoiceMessages = toCollection(recordings), toCollection(calls), toCollection(voiceMessages)
	for i, app := range apps {
		result.ScreenshotApps[i] = toCollection(app)
	}
	return result
}

func toCollection(c media.Collection) *model.Collection {
	return &model.Collection{Name: c.Name, Files: c.Files, Imported: c.Imported, Hidden: c.Hidden}
}
