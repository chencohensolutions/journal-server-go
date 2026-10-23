// Command scan-nudity scores every photo and video in a user's data folder for nudity, as the server does with new
// files (package nudity: NudeNet's detector on ONNX Runtime, no Python), and writes the score into the file's
// sidecar as "detected": {"nudity": {"score", "model", "scannedAt"}}. The user's tags are never touched; the server
// hides what scores at the user's choice or more (Hidden collections), and devices learn of it on their next sync.
//
// The server scans new files by itself; this is for scanning a data folder while the server isn't running, or all
// of it again (--rescan) after the model changed. Each file is read from a 640 px JPEG preview of it (a video's
// poster frame), made with libvips (and ffmpeg), never from the original (media.ModelPreview). Files already
// scanned by the same model are skipped, so running again continues where it stopped (Ctrl+C finishes the files in
// hand).
//
// The server may keep running: they take turns at writing to the data folder (media/turns.go). Run from the server
// folder:
//
//	go run ./scripts/scan-nudity --user hen [--rescan]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/media"
	"github.com/journal/server-go/nudity"
	"github.com/journal/server-go/onnx"
)

func main() {
	user := flag.String("user", "", "user whose data folder is scanned (from config.json)")
	rescan := flag.Bool("rescan", false, "scan files already scanned by this model again")
	jobs := flag.Int("jobs", max(1, runtime.NumCPU()/2), "previews made at once")
	configPath := flag.String("config", "config.json", "the server's config file")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/scan-nudity --user <name> [--rescan]")
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
	withVideos := true
	if ffmpeg, ffprobe, err := media.FindFFmpeg(config.FFmpeg); err == nil {
		previews = previews.WithFFmpeg(ffmpeg, ffprobe)
	} else {
		withVideos = false
		log.Printf("%v: videos are skipped (brew install ffmpeg; on Windows winget install Gyan.FFmpeg)", err)
	}
	if _, err := os.Stat(config.NudityModel); err != nil {
		log.Fatalf("no model at %s (go run ./scripts/bundle-onnx, or set \"nudity_model\" in config.json)", config.NudityModel)
	}
	path, err := onnx.FindRuntime(config.OnnxRuntime)
	check(err)
	rt, err := onnx.LoadRuntime(path)
	check(err)
	model, err := nudity.LoadModel(rt, config.NudityModel, config.SearchThreads)
	check(err)
	store, err := media.OpenStore(dataFolder, time.Now)
	check(err)

	todo := nudity.ToScan(store, *rescan, func(item media.Item) bool {
		return item.Sidecar.Facts.Kind == media.KindVideo && !withVideos
	})
	files := 0
	store.Range(func(media.Item) { files++ })
	fmt.Printf("%d files, %d to scan with %s\n", files, len(todo), nudity.ModelName)
	if len(todo) == 0 {
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var scanned, flagged, failed int
	started, lastProgress := time.Now(), time.Now()
	nudity.Scan(ctx, model, store, previews, todo, *jobs, func(item media.Item, result nudity.Result, err error) {
		switch {
		case errors.Is(err, context.Canceled):
			return
		case err != nil:
			log.Printf("%s: %v", item.Path, err)
			failed++
		default:
			scanned++
			if result.Score >= media.DefaultNudityFrom {
				flagged++
				fmt.Printf("%.2f  %s  %v\n", result.Score, item.Path, result.Found)
			}
		}
		if time.Since(lastProgress) > 10*time.Second {
			lastProgress = time.Now()
			fmt.Printf("… %d of %d scanned (%.1f/s)\n", scanned+failed, len(todo), float64(scanned+failed)/time.Since(started).Seconds())
		}
	})
	fmt.Printf("%d scanned, %d at %.1f or more, %d failed", scanned, flagged, media.DefaultNudityFrom, failed)
	if ctx.Err() != nil {
		fmt.Print(" (stopped; run again to continue)")
	}
	fmt.Println()
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
