// Command read-cameras reads which camera took each photo and video in a user's data folder, from its tags
// (media.ReadCamera: EXIF Make and Model, a DNG's own EXIF, a video's Android or QuickTime tags), and writes it into
// the file's sidecar as facts.camera, for files placed before the server read it on arrival. Only facts.camera
// changes, one turn per file; a file that has it already is skipped, so running again continues where it stopped
// (Ctrl+C finishes the files in hand). Originals are never touched, and devices aren't sent the files again: a
// file's camera is read when its info is shown.
//
// Photos are read by libvips inside this program (a few milliseconds each), as the server reads them; only with
// "vips_as_program" in config.json by the vipsheader program (about 350 ms a file on Windows). Videos are read with
// ffprobe, and skipped without it. It ends with every camera model and its files, how many of them were imported,
// and their years, which is what the user marks their own cameras from (cameras / setCamera), and how many files
// have no camera tags. --dry-run reads the tags and reports, writing nothing at all (not even the caches), so it is
// safe beside the running server; a real run may run beside it too (media/turns.go). Run from the server folder,
// before scripts/fix-sources:
//
//	go run ./scripts/read-cameras --user hen [--dry-run]
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/media"
)

func main() {
	user := flag.String("user", "", "user whose data folder is read (from config.json)")
	dryRun := flag.Bool("dry-run", false, "only read and report; writes nothing")
	jobs := flag.Int("jobs", max(1, runtime.NumCPU()/2), "files read at once")
	configPath := flag.String("config", "config.json", "the server's config file")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/read-cameras --user <name> [--dry-run]")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *user == "" || flag.NArg() != 0 || *jobs < 1 {
		flag.Usage()
		os.Exit(2)
	}

	config, err := auth.LoadConfig(*configPath)
	check(err)
	dataFolder, err := config.DataFolderOf(*user)
	check(err)
	vips, vipsheader, err := media.FindVips(config.Vips)
	if err != nil {
		log.Fatalf("%v (brew install vips; on Windows go run ./scripts/bundle-vips; or set \"vips\" in config.json)", err)
	}
	previews := media.NewPreviews(vips, vipsheader)
	if config.VipsAsProgram {
		log.Printf("Photos read with %s (\"vips_as_program\"): about a third of a second each", vipsheader)
	} else if version, err := previews.LoadLibrary(config.LibVips, config.SearchThreads); err != nil {
		log.Fatalf("libvips can't be loaded into this program (%v); set \"vips_as_program\": true in config.json to read "+
			"with %s instead, at about a third of a second a file", err, vipsheader)
	} else {
		log.Printf("Photos read with libvips %s inside this program", version)
	}
	tools := previews.CameraTools()
	withVideos := true
	if _, ffprobe, err := media.FindFFmpeg(config.FFmpeg); err == nil {
		tools = tools.WithFFprobe(ffprobe)
	} else {
		withVideos = false
		log.Printf("%v: videos are skipped (brew install ffmpeg; on Windows winget install Gyan.FFmpeg)", err)
	}
	open := media.OpenStore
	if *dryRun {
		open = media.OpenStoreReadOnly
	}
	store, err := open(dataFolder, time.Now)
	check(err)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	r, err := read(ctx, store, tools, withVideos, *dryRun, *jobs, os.Stdout)
	r.print(os.Stdout, *dryRun)
	check(err)
	if ctx.Err() != nil {
		fmt.Println("stopped; run again to continue")
	}
}

// model is one camera model in the report.
type model struct {
	camera          media.Camera
	files, imported int
	years           map[int]bool
}

// report is what a run found.
type report struct {
	files, had, read, written int
	noTags                    map[media.Kind]int
	skipped                   map[string]int // files not read, by why
	models                    map[string]*model
}

func (r *report) count(item media.Item, c media.Camera) {
	key := strings.ToLower(strings.TrimSpace(c.Make)) + "\x00" + strings.ToLower(strings.TrimSpace(c.Model))
	m := r.models[key]
	if m == nil {
		m = &model{camera: c, years: map[int]bool{}}
		r.models[key] = m
	}
	m.files++
	if item.Sidecar.Facts.Imported() {
		m.imported++
	}
	if t := item.Sidecar.Facts.TakenAt; t != nil {
		m.years[t.Year()] = true
	}
}

// read reads the camera of every file that has none in its sidecar, jobs at a time, and unless dryRun writes it.
func read(ctx context.Context, store *media.Store, tools media.CameraTools, withVideos, dryRun bool, jobs int, progress io.Writer) (*report, error) {
	r := &report{noTags: map[media.Kind]int{}, skipped: map[string]int{}, models: map[string]*model{}}
	var todo []media.Item
	store.Range(func(item media.Item) {
		r.files++
		facts := item.Sidecar.Facts
		switch {
		case facts.Camera != nil:
			r.had++
			r.count(item, *facts.Camera)
		case facts.Kind == media.KindAudio:
			r.skipped["sound recordings"]++
		case facts.Kind == media.KindVideo && !withVideos:
			r.skipped["videos (no ffprobe)"]++
		default:
			todo = append(todo, item)
		}
	})
	sort.Slice(todo, func(i, j int) bool { return todo[i].Path < todo[j].Path })
	fmt.Fprintf(progress, "%d files, %d with their camera already, %d to read\n", r.files, r.had, len(todo))

	type result struct {
		item   media.Item
		camera *media.Camera
	}
	next, results := make(chan media.Item), make(chan result)
	var wg sync.WaitGroup
	for range min(jobs, max(1, len(todo))) {
		wg.Go(func() {
			for item := range next {
				path := filepath.Join(store.Root(), filepath.FromSlash(item.Path))
				results <- result{item, media.ReadCamera(ctx, tools, path, item.Sidecar.Facts.Kind)}
			}
		})
	}
	go func() {
		defer close(next)
		for _, item := range todo {
			select {
			case next <- item:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	var err error
	started, lastProgress := time.Now(), time.Now()
	for res := range results {
		if ctx.Err() != nil {
			continue // stopped: what was read but not written is read again next time
		}
		r.read++
		if res.camera == nil {
			r.noTags[res.item.Sidecar.Facts.Kind]++
		} else {
			r.count(res.item, *res.camera)
			if !dryRun && err == nil {
				var changed bool
				if changed, err = store.SetCameraFact(res.item.Sha256(), *res.camera); err != nil {
					err = fmt.Errorf("%s: %w", res.item.Path, err)
				} else if changed {
					r.written++
				}
			}
		}
		if time.Since(lastProgress) > 10*time.Second {
			lastProgress = time.Now()
			fmt.Fprintf(progress, "… %d of %d read (%.0f/s)\n", r.read, len(todo), float64(r.read)/time.Since(started).Seconds())
		}
	}
	return r, err
}

// print lists every camera model, most files first, then the files without tags.
func (r *report) print(out io.Writer, dryRun bool) {
	if r == nil {
		return
	}
	if dryRun {
		fmt.Fprintf(out, "%d files read, %d cameras found (dry run, nothing written)\n\n", r.read, r.read-r.noTags[media.KindPhoto]-r.noTags[media.KindVideo])
	} else {
		fmt.Fprintf(out, "%d files read, %d cameras written\n\n", r.read, r.written)
	}
	models := make([]*model, 0, len(r.models))
	for _, m := range r.models {
		models = append(models, m)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].files != models[j].files {
			return models[i].files > models[j].files
		}
		return name(models[i].camera) < name(models[j].camera)
	})
	fmt.Fprintf(out, "%d camera models:\n", len(models))
	for _, m := range models {
		fmt.Fprintf(out, "  %8d  %s  (%d imported; %s)\n", m.files, name(m.camera), m.imported, years(m.years))
	}
	fmt.Fprintln(out, "no camera tags:")
	for _, kind := range []media.Kind{media.KindPhoto, media.KindVideo} {
		fmt.Fprintf(out, "  %8d  %ss\n", r.noTags[kind], kind)
	}
	for _, why := range []string{"sound recordings", "videos (no ffprobe)"} {
		if n := r.skipped[why]; n > 0 {
			fmt.Fprintf(out, "not read: %d %s\n", n, why)
		}
	}
}

// name is a camera as people say it: its model, with the make first when the model doesn't start with it
// ("samsung SM-S931B", "Canon EOS 80D").
func name(c media.Camera) string {
	switch {
	case c.Make == "":
		return c.Model
	case c.Model == "":
		return c.Make
	case strings.HasPrefix(strings.ToLower(c.Model), strings.ToLower(c.Make)):
		return c.Model
	}
	return c.Make + " " + c.Model
}

// years is a set of years as ranges: "2014–2016, 2019"; "no dates" when empty.
func years(set map[int]bool) string {
	if len(set) == 0 {
		return "no dates"
	}
	list := make([]int, 0, len(set))
	for y := range set {
		list = append(list, y)
	}
	sort.Ints(list)
	var parts []string
	for i := 0; i < len(list); {
		j := i
		for j+1 < len(list) && list[j+1] == list[j]+1 {
			j++
		}
		if i == j {
			parts = append(parts, fmt.Sprint(list[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d–%d", list[i], list[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
