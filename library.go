package main

import (
	"log"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/media"
	"github.com/journal/server-go/search"
)

// openLibrary opens the data folder of every user that has one configured.
func openLibrary(config auth.AppConfig) (*media.Library, error) {
	folders := map[string]string{}
	for _, user := range config.Users {
		if user.DataFolder != "" {
			folders[user.Username] = user.DataFolder
		} else {
			log.Printf("User %s has no data_folder in config.json; uploads are refused for them", user.Username)
		}
	}
	library, err := media.OpenLibrary(folders, time.Now)
	if err != nil {
		return nil, err
	}
	for _, user := range config.Users {
		if store := library.Store(user.Username); store != nil {
			store.CachePreviews = user.PreviewCache
		}
	}
	return library, nil
}

// newPreviews finds libvips and ffmpeg; without them the server runs, but previews (or video posters)
// answer 503.
func newPreviews(config auth.AppConfig) *media.Previews {
	vips, vipsheader, err := media.FindVips(config.Vips)
	if err != nil {
		log.Printf("Previews are off: %v (brew install vips; on Windows go run ./scripts/bundle-vips; or set \"vips\" in config.json)", err)
	} else {
		log.Printf("Previews made with %s", vips)
	}
	previews := media.NewPreviews(vips, vipsheader)
	ffmpeg, ffprobe, err := media.FindFFmpeg(config.FFmpeg)
	if err != nil {
		log.Printf("Video posters are off: %v (brew install ffmpeg; on Windows winget install Gyan.FFmpeg; or set \"ffmpeg\" in config.json)", err)
	} else {
		log.Printf("Video posters made with %s", ffmpeg)
		previews.WithFFmpeg(ffmpeg, ffprobe)
	}
	return previews
}

// newSearch is the search engine, when config.json turns search on; nil when off. Run starts it: the model loads
// in the background, and the server serves meanwhile.
func newSearch(config auth.AppConfig, library *media.Library, previews *media.Previews) *search.Engine {
	if config.Search == nil {
		log.Printf("Search is off (set \"search\" in config.json; see docs/media.md)")
		return nil
	}
	command, err := search.PythonCommand(config.Search.Python, config.Search.Model)
	if err != nil {
		log.Printf("Search is off: %v", err)
		return nil
	}
	return search.New(library, config.Search.Model, command, search.FromPreviews(previews))
}

// maintainLibrary deletes expired uploads, trims the preview caches and refreshes the index caches, once an hour.
func maintainLibrary(config auth.AppConfig, library *media.Library) {
	for ; ; time.Sleep(time.Hour) {
		library.PurgeExpiredUploads()
		library.TrimPreviews(int64(config.PreviewCacheMaxGB * (1 << 30)))
		library.RefreshIndexCaches()
	}
}
