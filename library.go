package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/journal/server-go/aesthetic"
	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/faces"
	"github.com/journal/server-go/graph/resolver"
	"github.com/journal/server-go/localindex"
	"github.com/journal/server-go/media"
	"github.com/journal/server-go/nudity"
	"github.com/journal/server-go/ocr"
	"github.com/journal/server-go/onnx"
	"github.com/journal/server-go/reading"
	"github.com/journal/server-go/search"
	"github.com/journal/server-go/similar"
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
	// Uploaded sound recordings are read with ffprobe, and an uploaded WAV is kept as FLAC with ffmpeg.
	ffmpeg, ffprobe, _ := media.FindFFmpeg(config.FFmpeg)
	for _, user := range config.Users {
		if store := library.Store(user.Username); store != nil {
			store.CachePreviews = user.PreviewCache
			store.FFmpeg, store.FFprobe = ffmpeg, ffprobe
		}
	}
	return library, nil
}

// newPreviews finds libvips and ffmpeg; without them the server runs, but previews (or video posters)
// answer 503. libvips is loaded into the server (unless "vips_as_program"), with the vips programs kept for what
// it fails on.
func newPreviews(config auth.AppConfig) *media.Previews {
	vips, vipsheader, err := media.FindVips(config.Vips)
	if err != nil {
		log.Printf("Previews are off: %v (brew install vips; on Windows go run ./scripts/bundle-vips; or set \"vips\" in config.json)", err)
	}
	previews := media.NewPreviews(vips, vipsheader)
	switch {
	case err != nil:
	case config.VipsAsProgram:
		log.Printf("Previews made with %s (\"vips_as_program\")", vips)
	default:
		if version, err := previews.LoadLibrary(config.LibVips, config.SearchThreads); err != nil {
			log.Printf("Previews made with %s: libvips can't be loaded into the server: %v", vips, err)
		} else {
			log.Printf("Previews made with libvips %s inside the server (%d threads), and %s for what it can't make", version,
				config.SearchThreads, vips)
		}
	}
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
// is missing. The reader (newReader) reads the files that have no vector yet.
func newSearch(config auth.AppConfig, library *media.Library) *search.Service {
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
	service, err := search.NewService(model, library)
	if err != nil {
		log.Printf("Text search is off: %v", err)
		return nil
	}
	log.Printf("Text search with %s on ONNX Runtime %s (%d threads), loaded in %s", model.Name, rt.Version,
		config.SearchThreads, time.Since(started).Round(time.Millisecond))
	return service
}

// newFaces loads ONNX Runtime and the face models and opens each user's faces and people; nil (face search off)
// when either is missing. The reader (newReader) reads the files not read yet.
func newFaces(config auth.AppConfig, library *media.Library) *faces.Service {
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
	service, err := faces.NewService(model, library)
	if err != nil {
		log.Printf("Face search is off: %v", err)
		return nil
	}
	log.Printf("Face search with %s on ONNX Runtime %s (%d threads)", model.Name, rt.Version, config.SearchThreads)
	return service
}

// newNudity loads ONNX Runtime and NudeNet's detector; nil (no nudity scan) when either is missing. The reader
// (newReader) scans the files not scanned yet.
func newNudity(config auth.AppConfig) *nudity.Service {
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
	return nudity.NewService(model)
}

// newSimilar loads ONNX Runtime and DINOv2 and opens each user's vectors; nil (similar photos and duplicates off)
// when either is missing. The reader (newReader) reads the files not read yet.
func newSimilar(config auth.AppConfig, library *media.Library) *similar.Service {
	if _, err := os.Stat(config.SimilarModel); err != nil {
		log.Printf("Similar photos are off: no model in %s (go run ./scripts/bundle-onnx, or set \"similar_model\" in config.json)", config.SimilarModel)
		return nil
	}
	rt, err := loadRuntime(config)
	if err != nil {
		log.Printf("Similar photos are off: %v", err)
		return nil
	}
	model, err := similar.LoadModel(rt, config.SimilarModel, config.SearchThreads)
	if err != nil {
		log.Printf("Similar photos are off: %s: %v", config.SimilarModel, err)
		return nil
	}
	service, err := similar.NewService(model, library)
	if err != nil {
		log.Printf("Similar photos are off: %v", err)
		return nil
	}
	log.Printf("Similar photos and duplicates with %s on ONNX Runtime %s (%d threads)", model.Name, rt.Version, config.SearchThreads)
	return service
}

// newOCR loads ONNX Runtime and the OCR models and opens each user's texts; nil (text in photos off) when either is
// missing. The reader (newReader) reads the photos not read yet.
func newOCR(config auth.AppConfig, library *media.Library) *ocr.Service {
	if _, err := os.Stat(config.OCRModel); err != nil {
		log.Printf("Text in photos is off: no models in %s (go run ./scripts/bundle-onnx, or set \"ocr_model\" in config.json)", config.OCRModel)
		return nil
	}
	rt, err := loadRuntime(config)
	if err != nil {
		log.Printf("Text in photos is off: %v", err)
		return nil
	}
	model, err := ocr.LoadModel(rt, config.OCRModel, config.SearchThreads)
	if err != nil {
		log.Printf("Text in photos is off: %s: %v", config.OCRModel, err)
		return nil
	}
	service, err := ocr.NewService(model, library)
	if err != nil {
		log.Printf("Text in photos is off: %v", err)
		return nil
	}
	log.Printf("Text in photos with %s on ONNX Runtime %s (%d threads)", model.Name, rt.Version, config.SearchThreads)
	return service
}

// newAesthetic loads ONNX Runtime, CLIP ViT-L/14 and LAION's head and opens each user's scores; nil (aesthetic
// scores off) when either is missing. The reader (newReader) reads the files not read yet.
func newAesthetic(config auth.AppConfig, library *media.Library) *aesthetic.Service {
	if _, err := os.Stat(config.AestheticModel); err != nil {
		log.Printf("Aesthetic scores are off: no model in %s (go run ./scripts/bundle-onnx, or set \"aesthetic_model\" in config.json)", config.AestheticModel)
		return nil
	}
	rt, err := loadRuntime(config)
	if err != nil {
		log.Printf("Aesthetic scores are off: %v", err)
		return nil
	}
	model, err := aesthetic.LoadModel(rt, config.AestheticModel, config.SearchThreads)
	if err != nil {
		log.Printf("Aesthetic scores are off: %s: %v", config.AestheticModel, err)
		return nil
	}
	service, err := aesthetic.NewService(model, library)
	if err != nil {
		log.Printf("Aesthetic scores are off: %v", err)
		return nil
	}
	log.Printf("Aesthetic scores with %s on ONNX Runtime %s (%d threads)", model.Name, rt.Version, config.SearchThreads)
	return service
}

// indexes is what the server keeps about each file, each nil when it is off (no model), and the reader that reads
// the files for them.
type indexes struct {
	nudity    *nudity.Service
	faces     *faces.Service
	ocr       *ocr.Service
	search    *search.Service
	similar   *similar.Service
	aesthetic *aesthetic.Service
	reader    *reading.Reader
	// The caches are written by journal-server index ("index": "daily"), and read here to answer queries.
	daily bool
	// Starts, follows and pauses each user's reading (indexrun.go), for the indexing fields; nil: none.
	runs resolver.IndexRunner
}

// openIndexes is the server's indexes. With "index": "server" (the default) every index whose model is there,
// and the reader for them; with "index": "daily" only what queries need (openQueryIndexes), the reading being
// done by journal-server index once a day.
func openIndexes(config auth.AppConfig, library *media.Library, previews *media.Previews) indexes {
	if config.Index == auth.IndexDaily {
		return openQueryIndexes(config, library)
	}
	return loadIndexes(config, library, previews, nil)
}

// openQueryIndexes is what queries need of the indexes another process writes (journal-server index): the text
// half of the search model, and each index's caches opened to read only (refresh reads what the process added).
// No reading model is loaded and there is no reader. An index whose model folder isn't there is off, as when it
// reads in the server.
func openQueryIndexes(config auth.AppConfig, library *media.Library) indexes {
	var ix indexes
	named := func(folder, what, setting string) (string, bool) {
		abs, err := filepath.Abs(folder)
		if _, statErr := os.Stat(folder); err != nil || statErr != nil {
			log.Printf("%s off: no model in %s (go run ./scripts/bundle-onnx, or set \"%s\" in config.json)", what, folder, setting)
			return "", false
		}
		return filepath.Base(abs), true
	}
	fail := func(what string, err error) { log.Printf("%s off: %v", what, err) }
	if name, ok := named(config.SearchModel, "Text search is", "search_model"); ok {
		// The text half (1.15 GB) is loaded only while searches come: about 4 s at the first after a quiet spell.
		rt, err := loadRuntime(config)
		if err == nil {
			ix.search, err = search.NewOnDemandService(rt, config.SearchModel, config.SearchThreads, library)
		}
		if err != nil {
			fail("Text search is", fmt.Errorf("%s: %w", name, err))
		}
	}
	var err error
	if name, ok := named(config.FaceModel, "Face search is", "face_model"); ok {
		if ix.faces, err = faces.NewReadOnlyService(name, library); err != nil {
			fail("Face search is", err)
		}
	}
	if name, ok := named(config.OCRModel, "Text in photos is", "ocr_model"); ok {
		if ix.ocr, err = ocr.NewReadOnlyService(name, library); err != nil {
			fail("Text in photos is", err)
		}
	}
	if name, ok := named(config.SimilarModel, "Similar photos are", "similar_model"); ok {
		if ix.similar, err = similar.NewReadOnlyService(name, library); err != nil {
			fail("Similar photos are", err)
		}
	}
	if name, ok := named(config.AestheticModel, "Aesthetic scores are", "aesthetic_model"); ok {
		if ix.aesthetic, err = aesthetic.NewReadOnlyService(name, library); err != nil {
			fail("Aesthetic scores are", err)
		}
	}
	ix.daily = true
	// Reading the caches left garbage behind: give it back to the system now, not in the minutes the runtime takes.
	debug.FreeOSMemory()
	log.Printf("Indexing daily at %s in a separate process (journal-server index); the server keeps only what queries need", config.IndexAt)
	return ix
}

// refresh reads what journal-server index added to the caches (with "index": "daily"); a no-op otherwise.
func (ix indexes) refresh() {
	if !ix.daily {
		return
	}
	var errs []error
	if ix.search != nil {
		errs = append(errs, ix.search.Refresh())
	}
	if ix.faces != nil {
		errs = append(errs, ix.faces.Refresh())
	}
	if ix.ocr != nil {
		errs = append(errs, ix.ocr.Refresh())
	}
	if ix.similar != nil {
		errs = append(errs, ix.similar.Refresh())
	}
	if ix.aesthetic != nil {
		errs = append(errs, ix.aesthetic.Refresh())
	}
	if err := errors.Join(errs...); err != nil {
		log.Printf("Reading the indexes' new results: %v", err)
	}
	debug.FreeOSMemory()
}

// newLocal is what phones may keep of the indexes that are on: each one's cache, the people of the face index, and
// the files a phone runs text search with (SigLIP's text half, fp32 or q8, and its tokenizer).
func newLocal(config auth.AppConfig, library *media.Library, ix indexes) *localindex.Server {
	var sources []localindex.Source
	var people localindex.Members
	if ix.search != nil {
		sources = append(sources, localindex.Source{Name: "search", Format: localindex.FormatVectors, Cache: ix.search,
			ModelFolder: config.SearchModel, ModelFiles: []localindex.ModelFileSpec{
				{Name: "text_model.onnx", Precision: "fp32"},
				{Name: "text_model.onnx_data", Precision: "fp32"}, // only the large model has one
				{Name: search.PhoneTextModelQ8, Precision: "q8"},
				{Name: "tokenizer.model"},
			}})
	}
	if ix.faces != nil {
		sources = append(sources, localindex.Source{Name: "faces", Format: localindex.FormatFaces, Cache: ix.faces})
		people = ix.faces
	}
	if ix.ocr != nil {
		sources = append(sources, localindex.Source{Name: "ocr", Format: localindex.FormatJSONL, Cache: ix.ocr})
	}
	if ix.similar != nil {
		sources = append(sources, localindex.Source{Name: "similar", Format: localindex.FormatVectors, Cache: ix.similar})
	}
	if ix.aesthetic != nil {
		sources = append(sources, localindex.Source{Name: "aesthetic", Format: localindex.FormatVectors, Cache: ix.aesthetic})
	}
	return localindex.New(library, people, sources...)
}

// close closes the indexes' cache files.
func (ix indexes) close() {
	if ix.search != nil {
		ix.search.Close()
	}
	if ix.faces != nil {
		ix.faces.Close()
	}
	if ix.ocr != nil {
		ix.ocr.Close()
	}
	if ix.similar != nil {
		ix.similar.Close()
	}
	if ix.aesthetic != nil {
		ix.aesthetic.Close()
	}
}

// indexNames are the indexes by the names journal-server index --only takes, in the order they read a file.
var indexNames = []string{"nudity", "faces", "ocr", "search", "similar", "aesthetic"}

// loadIndexes loads every index whose model is there, and the reader for them; with only, just the indexes it names
// (journal-server index --only), so the others' models take no memory.
func loadIndexes(config auth.AppConfig, library *media.Library, previews *media.Previews, only map[string]bool) indexes {
	want := func(name string) bool { return only == nil || only[name] }
	var ix indexes
	if want("nudity") {
		ix.nudity = newNudity(config)
	}
	if want("faces") {
		ix.faces = newFaces(config, library)
	}
	if want("ocr") {
		ix.ocr = newOCR(config, library)
	}
	if want("search") {
		ix.search = newSearch(config, library)
	}
	if want("similar") {
		ix.similar = newSimilar(config, library)
	}
	if want("aesthetic") {
		ix.aesthetic = newAesthetic(config, library)
	}
	ix.reader = newReader(config, library, previews, ix)
	return ix
}

// newReader reads the users' files for the indexes that are on: each file decoded once and given to all of them.
// nil when none is on.
func newReader(config auth.AppConfig, library *media.Library, previews *media.Previews, ix indexes) *reading.Reader {
	r := reading.New(library, reading.Previews{Previews: previews}, config.ReaderPrepare)
	if ix.nudity != nil {
		r.Add(ix.nudity)
	}
	if ix.faces != nil {
		r.Add(ix.faces.Index())
	}
	if ix.ocr != nil {
		r.Add(ix.ocr)
	}
	if ix.search != nil {
		r.Add(ix.search)
	}
	if ix.similar != nil {
		r.Add(ix.similar)
	}
	if ix.aesthetic != nil {
		r.Add(ix.aesthetic)
	}
	if len(r.Names()) == 0 {
		return nil
	}
	log.Printf("Reading new files with %s, %d ahead", strings.Join(r.Models(), ", "), config.ReaderPrepare)
	return r
}

// maintainLibrary deletes expired uploads, trims the preview caches and refreshes the index caches once an hour
// (catching what scripts changed too), and every few minutes saves the index cache of a data folder the server
// wrote into, so that a restart doesn't read the library's sidecars again.
func maintainLibrary(config auth.AppConfig, library *media.Library) {
	const every = 5 * time.Minute
	for tick := 0; ; tick++ {
		if tick%int(time.Hour/every) == 0 {
			library.PurgeExpiredUploads()
			library.TrimPreviews(int64(config.PreviewCacheMaxGB * (1 << 30)))
			library.RefreshIndexCaches()
		} else {
			library.RefreshIndexCachesIfChanged()
		}
		time.Sleep(every)
	}
}
