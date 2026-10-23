package main

import (
	"log"
	"os"
	"sync"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/faces"
	"github.com/journal/server-go/media"
	"github.com/journal/server-go/nudity"
	"github.com/journal/server-go/onnx"
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

var (
	runtimeOnce sync.Once
	runtimeLib  *onnx.Runtime
	runtimeErr  error
)

// loadRuntime loads ONNX Runtime, once for the process: text search, face search and the nudity scan share it.
func loadRuntime(config auth.AppConfig) (*onnx.Runtime, error) {
	runtimeOnce.Do(func() {
		var path string
		if path, runtimeErr = onnx.FindRuntime(config.OnnxRuntime); runtimeErr == nil {
			runtimeLib, runtimeErr = onnx.LoadRuntime(path)
		}
	})
	return runtimeLib, runtimeErr
}

// newSearch loads ONNX Runtime and the search model and opens each user's vectors; nil (search off) when either
// is missing. Its Run reads the files that have no vector yet.
func newSearch(config auth.AppConfig, library *media.Library, previews *media.Previews) *search.Service {
	if _, err := os.Stat(config.SearchModel); err != nil {
		log.Printf("Text search is off: no model in %s (go run ./scripts/bundle-onnx, or set \"search_model\" in config.json)", config.SearchModel)
		return nil
	}
	rt, err := loadRuntime(config)
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

// newFaces loads ONNX Runtime and the face models and opens each user's faces and people; nil (face search off)
// when either is missing. Its Run reads the files not read yet.
func newFaces(config auth.AppConfig, library *media.Library, previews *media.Previews) *faces.Service {
	if _, err := os.Stat(config.FaceModel); err != nil {
		log.Printf("Face search is off: no models in %s (go run ./scripts/bundle-onnx, or set \"face_model\" in config.json)", config.FaceModel)
		return nil
	}
	rt, err := loadRuntime(config)
	if err != nil {
		log.Printf("Face search is off: %v", err)
		return nil
	}
	model, err := faces.LoadModel(rt, config.FaceModel, config.SearchThreads)
	if err != nil {
		log.Printf("Face search is off: %s: %v", config.FaceModel, err)
		return nil
	}
	service, err := faces.NewService(model, library, previews)
	if err != nil {
		log.Printf("Face search is off: %v", err)
		return nil
	}
	log.Printf("Face search with %s on ONNX Runtime %s (%d threads)", model.Name, rt.Version, config.SearchThreads)
	return service
}

// newNudity loads ONNX Runtime and NudeNet's detector; nil (no nudity scan) when either is missing. Its Run scans
// the files not scanned yet.
func newNudity(config auth.AppConfig, library *media.Library, previews *media.Previews) *nudity.Service {
	if _, err := os.Stat(config.NudityModel); err != nil {
		log.Printf("The nudity scan is off: no model at %s (go run ./scripts/bundle-onnx, or set \"nudity_model\" in config.json)", config.NudityModel)
		return nil
	}
	rt, err := loadRuntime(config)
	if err != nil {
		log.Printf("The nudity scan is off: %v", err)
		return nil
	}
	model, err := nudity.LoadModel(rt, config.NudityModel, config.SearchThreads)
	if err != nil {
		log.Printf("The nudity scan is off: %v", err)
		return nil
	}
	log.Printf("Nudity scan with %s on ONNX Runtime %s (%d threads)", nudity.ModelName, rt.Version, config.SearchThreads)
	return nudity.NewService(model, library, previews)
}

// maintainLibrary deletes expired uploads, trims the preview caches and refreshes the index caches, once an hour.
func maintainLibrary(config auth.AppConfig, library *media.Library) {
	for ; ; time.Sleep(time.Hour) {
		library.PurgeExpiredUploads()
		library.TrimPreviews(int64(config.PreviewCacheMaxGB * (1 << 30)))
		library.RefreshIndexCaches()
	}
}
