package main

import (
	"log"
	"os"
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

// newSearch loads ONNX Runtime and the search model and opens each user's vectors; nil (search off) when either
// is missing. Its Run reads the files that have no vector yet.
func newSearch(config auth.AppConfig, library *media.Library, previews *media.Previews) *search.Service {
	if _, err := os.Stat(config.SearchModel); err != nil {
		log.Printf("Text search is off: no model in %s (go run ./scripts/bundle-onnx, or set \"search_model\" in config.json)", config.SearchModel)
		return nil
	}
	path, err := search.FindRuntime(config.OnnxRuntime)
	if err != nil {
		log.Printf("Text search is off: %v", err)
		return nil
	}
	rt, err := search.LoadRuntime(path)
	if err != nil {
		log.Printf("Text search is off: %v", err)
		return nil
	}
	started := time.Now()
	model, err := search.LoadModel(rt, config.SearchModel, config.SearchThreads)
	if err != nil {
		log.Printf("Text search is off: %s: %v", config.SearchModel, err)
		return nil
	}
	service, err := search.NewService(model, library, previews)
	if err != nil {
		log.Printf("Text search is off: %v", err)
		return nil
	}
	log.Printf("Text search with %s on ONNX Runtime %s (%d threads), loaded in %s", model.Name, rt.Version,
		config.SearchThreads, time.Since(started).Round(time.Millisecond))
	return service
}

// maintainLibrary deletes expired uploads, trims the preview caches and refreshes the index caches, once an hour.
func maintainLibrary(config auth.AppConfig, library *media.Library) {
	for ; ; time.Sleep(time.Hour) {
		library.PurgeExpiredUploads()
		library.TrimPreviews(int64(config.PreviewCacheMaxGB * (1 << 30)))
		library.RefreshIndexCaches()
	}
}
