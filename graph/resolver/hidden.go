package resolver

import (
	"strings"

	"github.com/journal/server-go/graph/model"
	"github.com/journal/server-go/media"
)

func hiddenCollections(store *media.Store) *model.HiddenCollections {
	whatsapp, withoutApp, apps := store.Collections()
	folders := store.AppCollections()
	result := &model.HiddenCollections{
		Whatsapp:              toCollection(whatsapp),
		Apps:                  make([]*model.Collection, len(folders)),
		ScreenshotsWithoutApp: toCollection(withoutApp),
		ScreenshotApps:        make([]*model.Collection, len(apps)),
	}
	for i, folder := range folders {
		result.Apps[i] = toCollection(folder)
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
	result.SentToYou = toCollection(store.SentToYouCollection())
	for i, app := range apps {
		result.ScreenshotApps[i] = toCollection(app)
	}
	return result
}

func toCollection(c media.Collection) *model.Collection {
	return &model.Collection{Name: c.Name, Files: c.Files, Imported: c.Imported, Hidden: c.Hidden}
}

// hiddenSources is the sources setHiddenCollections hides: the folder kinds apps names (or, left out, those hidden
// now), with WhatsApp added or taken out when whatsapp says (an app from before apps sends only whatsapp), and the
// other sources hidden now kept; received (Sent to you) is added or taken out when sentToYou says, and kept as it is
// when left out.
func hiddenSources(current, apps []string, whatsapp, sentToYou *bool) []string {
	var sources []string
	for _, s := range current {
		if apps == nil || media.FolderName(s) == "" {
			sources = append(sources, s)
		}
	}
	for _, app := range apps {
		app = strings.TrimSpace(app)
		if media.FolderName(app) == "" {
			app = media.FolderSource(app)
		}
		if kind := media.NormalizeSource(app); media.FolderName(kind) != "" {
			sources = append(sources, kind)
		}
	}
	if whatsapp != nil {
		kept := sources[:0]
		for _, s := range sources {
			if !media.SameSource(s, media.SourceWhatsApp) {
				kept = append(kept, s)
			}
		}
		sources = kept
		if *whatsapp {
			sources = append(sources, media.SourceWhatsApp)
		}
	}
	if sentToYou != nil {
		kept := make([]string, 0, len(sources)+1)
		for _, s := range sources {
			if !media.SameSource(s, media.SourceReceived) {
				kept = append(kept, s)
			}
		}
		sources = kept
		if *sentToYou {
			sources = append(sources, media.SourceReceived)
		}
	}
	return sources
}
