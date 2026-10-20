// Command scan-nudity scores every photo and video in a user's data folder for nudity with NudeNet, and writes the
// score into the file's sidecar as "detected": {"nudity": {"score", "model", "scannedAt"}}. The user's tags are
// never touched: clients hide what is over the user's threshold ("Show private media" off) and fetch nothing of it.
// A scanned file comes again in the change feed, so devices learn its score on their next sync.
//
// NudeNet runs in Python, as a separate program (like libvips and ffmpeg): python3 -m pip install nudenet.
// It is given a 640 px JPEG preview of each photo, and of each video's poster frame, made with libvips (and ffmpeg),
// never the original. Files already scanned by the same model are skipped, so running again continues where it
// stopped (Ctrl+C finishes the files in hand); --rescan scans everything again.
//
// Stop the server first: it reads the data folder only when it starts. Run from the server folder:
//
//	go run ./scripts/scan-nudity --user hen [--rescan] [--python python3]
package main

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/media"
)

// model names the detector in the sidecar; change it when the worker changes how it scores, so files scan again.
const model = "nudenet-320n"

// The score from which a file is counted as flagged in the report. Only for the report: clients use their own.
const reportThreshold = 0.5

//go:embed nudenet_worker.py
var workerScript []byte

// previewSpec is what NudeNet is given: small, since it scales to 320 px itself.
var previewSpec = media.PreviewSpec{Width: 640, Height: 640, Fit: "contain", Format: "jpeg", Quality: "high"}

func main() {
	user := flag.String("user", "", "user whose data folder is scanned (from config.json)")
	rescan := flag.Bool("rescan", false, "scan files already scanned by this model again")
	jobs := flag.Int("jobs", max(1, runtime.NumCPU()/2), "previews made at once")
	python := flag.String("python", defaultPython(), "the Python that has NudeNet (python3 -m pip install nudenet)")
	configPath := flag.String("config", "config.json", "the server's config file")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/scan-nudity --user <name> [--rescan] [--python <path>]")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *user == "" || flag.NArg() != 0 || *jobs < 1 {
		flag.Usage()
		os.Exit(2)
	}

	config, err := loadConfig(*configPath)
	check(err)
	var dataFolder string
	for _, u := range config.Users {
		if u.Username == *user {
			dataFolder = u.DataFolder
		}
	}
	if dataFolder == "" {
		log.Fatalf("%s: user %q has no data_folder", *configPath, *user)
	}
	if conn, err := net.DialTimeout("tcp", config.AdminAddr, time.Second); err == nil {
		conn.Close()
		log.Fatalf("the server is running (its admin API answers on %s); stop it first, it reads the data folder only when it starts", config.AdminAddr)
	}
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
	store, err := media.OpenStore(dataFolder, time.Now)
	check(err)

	var todo []media.Item
	items, _, _, err := store.Changes("", 1<<31-1)
	check(err)
	for _, item := range items {
		if item.Sidecar.Facts.Kind == media.KindVideo && !withVideos {
			continue
		}
		if d := item.Sidecar.Detected; *rescan || d == nil || d.Nudity == nil || d.Nudity.Model != model {
			todo = append(todo, item)
		}
	}
	// Newest first: the photos a device shows first are scanned first.
	sort.Slice(todo, func(i, j int) bool { return todo[i].Sidecar.Facts.UploadedAt.After(todo[j].Sidecar.Facts.UploadedAt) })
	fmt.Printf("%d files, %d to scan with %s\n", len(items), len(todo), model)
	if len(todo) == 0 {
		return
	}

	worker, err := startWorker(*python)
	check(err)
	defer worker.close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	type ready struct {
		item    media.Item
		path    string
		release func()
		err     error
	}
	queue := make(chan media.Item)
	made := make(chan ready, *jobs)
	go func() {
		defer close(queue)
		for _, item := range todo {
			select {
			case queue <- item:
			case <-ctx.Done():
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for range *jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range queue {
				path, release, err := previews.Get(ctx, store, item, previewSpec)
				made <- ready{item, path, release, err}
			}
		}()
	}
	go func() { wg.Wait(); close(made) }()

	var scanned, flagged, failed int
	started, lastProgress := time.Now(), time.Now()
	for r := range made {
		if r.err != nil {
			if !errors.Is(r.err, context.Canceled) {
				log.Printf("%s: no preview: %v", r.item.Path, r.err)
				failed++
			}
			continue
		}
		answer, err := worker.score(r.path)
		r.release()
		if err != nil {
			log.Fatalf("NudeNet stopped: %v", err)
		}
		if answer.Error != "" {
			log.Printf("%s: %s", r.item.Path, answer.Error)
			failed++
			continue
		}
		check(store.SetNudity(r.item.Sha256(), media.Detection{Score: answer.Score, Model: model, ScannedAt: time.Now().UTC()}))
		scanned++
		if answer.Score >= reportThreshold {
			flagged++
			fmt.Printf("%.2f  %s  %v\n", answer.Score, r.item.Path, answer.Found)
		}
		if time.Since(lastProgress) > 10*time.Second {
			lastProgress = time.Now()
			fmt.Printf("… %d of %d scanned (%.1f/s)\n", scanned+failed, len(todo), float64(scanned+failed)/time.Since(started).Seconds())
		}
	}
	fmt.Printf("%d scanned, %d at %.1f or more, %d failed", scanned, flagged, reportThreshold, failed)
	if ctx.Err() != nil {
		fmt.Print(" (stopped; run again to continue)")
	}
	fmt.Println()
}

// worker is the Python NudeNet process: a picture path in, a JSON answer out, one line each.
type worker struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Scanner
	script string
}

type answer struct {
	Ready bool     `json:"ready"`
	Score float64  `json:"score"`
	Found []string `json:"found"`
	Error string   `json:"error"`
}

func startWorker(python string) (*worker, error) {
	script, err := os.CreateTemp("", "nudenet-worker-*.py")
	if err != nil {
		return nil, err
	}
	if _, err := script.Write(workerScript); err != nil {
		return nil, err
	}
	script.Close()
	cmd := exec.Command(python, script.Name())
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		os.Remove(script.Name())
		return nil, fmt.Errorf("starting %s: %w (or pass --python)", python, err)
	}
	w := &worker{cmd: cmd, in: in, out: bufio.NewScanner(out), script: script.Name()}
	if first, err := w.read(); err != nil || !first.Ready {
		w.close()
		return nil, fmt.Errorf("NudeNet didn't start (python3 -m pip install nudenet): %v", err)
	}
	return w, nil
}

func (w *worker) score(path string) (answer, error) {
	if _, err := fmt.Fprintln(w.in, path); err != nil {
		return answer{}, err
	}
	return w.read()
}

func (w *worker) read() (answer, error) {
	if !w.out.Scan() {
		if err := w.out.Err(); err != nil {
			return answer{}, err
		}
		return answer{}, io.ErrUnexpectedEOF
	}
	var a answer
	err := json.Unmarshal(w.out.Bytes(), &a)
	return a, err
}

func (w *worker) close() {
	w.in.Close()
	w.cmd.Wait()
	os.Remove(w.script)
}

func defaultPython() string {
	if runtime.GOOS == "windows" {
		return "python"
	}
	return "python3"
}

func loadConfig(path string) (auth.AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return auth.AppConfig{}, fmt.Errorf("%w (run from the server folder, or pass --config)", err)
	}
	var config auth.AppConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return auth.AppConfig{}, fmt.Errorf("%s: %w", path, err)
	}
	return config.WithDefaults(), nil
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
