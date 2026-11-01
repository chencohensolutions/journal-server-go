// Command jxl keeps a user's JPEG photos as JPEG XL, losslessly (the owner's choice, Oct 28, 2026): each JPEG is
// packed again with libjxl, about 20% smaller, and replaces the JPEG only when the very same JPEG, byte for byte,
// comes back from it (media.Store.KeepAsJXL). The JPEG XL's sidecar keeps the JPEG's sha256, size, type and name in
// encodedFrom, so phones and imports holding the JPEG find it, and GET /media/{the JPEG's sha256} still gives the JPEG.
// The server keeps new uploads so as they arrive; this does the files already there, and is made to run overnight:
// beside the running server (media/turns.go), --jobs files at once, saying every 10 seconds how far it is and how much
// smaller the JPEGs became, and stopping at --until or Ctrl+C after the files in hand. Running again continues where it
// stopped: each JPEG is replaced in one short turn, so even a run killed outright leaves every file whole. A JPEG libjxl can't keep (CMYK, arithmetic coding, a damaged file)
// stays, and is listed in .cache/jxl/failed.tsv so later runs skip it (--retry tries them again).
//
// It ends with what keeping JPEGs as JPEG XL has saved in the data folder, in all and in percent (also on its own,
// from the sidecars, with --report). --dry-run writes nothing at all: it packs --sample JPEGs in memory, spread over
// the library, and estimates what a run would save. Run from the server folder:
//
//	go run ./scripts/jxl --user hen --dry-run
//	go run ./scripts/jxl --user hen [--until 07:00] [--limit n] [--jobs n]
//	go run ./scripts/jxl --user hen --report
//
// The indexes (search, faces, and the others) give each JPEG XL the JPEG's results without reading it again (reading.Carrier):
// the server does so within a minute, or, with "index": "daily", journal-server index at its next run.
package main

import (
	"bufio"
	"context"
	"errors"
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
	"github.com/journal/server-go/version"
)

func main() {
	user := flag.String("user", "", "user whose data folder is converted (from config.json)")
	dryRun := flag.Bool("dry-run", false, "estimate what a run would save from a sample; writes nothing")
	sample := flag.Int("sample", 200, "with --dry-run, how many JPEGs are packed to estimate the saving")
	reportOnly := flag.Bool("report", false, "only report what has been saved so far")
	jobs := flag.Int("jobs", max(1, runtime.NumCPU()/2), "files packed at once")
	limit := flag.Int("limit", 0, "convert at most this many files in this run (0: all)")
	until := flag.String("until", "", "stop at this local time (HH:MM), finishing the files in hand")
	retry := flag.Bool("retry", false, "try again the JPEGs earlier runs could not keep")
	configPath := flag.String("config", "config.json", "the server's config file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/jxl --user <name> [--dry-run | --report] [--until HH:MM] [--limit n]")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println("journal-jxl", version.String())
		return
	}
	if *user == "" || flag.NArg() != 0 || *jobs < 1 || *sample < 1 {
		flag.Usage()
		os.Exit(2)
	}

	// Installed as journal-jxl, it may run from any folder (auth.UseInstallFolder).
	_, err := auth.UseInstallFolder(*configPath)
	check(err)
	config, err := auth.LoadConfig(*configPath)
	check(err)
	dataFolder, err := config.DataFolderOf(*user)
	check(err)
	open := media.OpenStore
	if *dryRun || *reportOnly {
		open = media.OpenStoreReadOnly
	}
	store, err := open(dataFolder, time.Now)
	check(err)
	if *reportOnly {
		savings(store).print(os.Stdout)
		return
	}
	vips, _, _ := media.FindVips(config.Vips)
	x, err := media.LoadJXL(media.JXLLibraries(vips, config.LibJXL))
	if err != nil {
		log.Fatalf("%v (it comes with libvips: go run ./scripts/bundle-vips on Windows, brew install vips; or set \"libjxl\" in config.json)", err)
	}
	log.Printf("libjxl %s", x.Version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *until != "" {
		at, err := nextTime(*until, time.Now())
		if err != nil {
			log.Fatalf("--until: %v", err)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, at)
		defer cancel()
		log.Printf("stopping at %s", at.Format("Mon 15:04"))
	}

	failed := readFailed(store)
	todo := jpegs(store, failed, *retry)
	if *dryRun {
		check(estimate(ctx, store, x, todo, *sample, *jobs, os.Stdout))
		return
	}
	store.JXL = x
	if *limit > 0 && len(todo) > *limit {
		todo = todo[:*limit]
	}
	run, err := convert(ctx, store, todo, *jobs, os.Stdout)
	run.print(os.Stdout)
	if len(run.failed) > 0 {
		if werr := writeFailed(store, run.failed); werr != nil {
			log.Printf("listing the JPEGs that stay: %v", werr)
		}
	}
	fmt.Println()
	savings(store).print(os.Stdout)
	check(err)
	if ctx.Err() != nil {
		fmt.Println("stopped; run again to continue")
	}
}

// jpegs is the JPEG photos still kept as JPEG, oldest month first, leaving out those earlier runs couldn't keep unless
// retry.
func jpegs(store *media.Store, failed map[string]string, retry bool) []media.Item {
	var todo []media.Item
	store.Range(func(item media.Item) {
		f := item.Sidecar.Facts
		if f.Kind != media.KindPhoto || f.EncodedFrom != nil || !media.IsJPEG(f.FileName, f.MimeType) {
			return
		}
		if _, ok := failed[item.Sha256()]; ok && !retry {
			return
		}
		todo = append(todo, item)
	})
	sort.Slice(todo, func(i, j int) bool { return todo[i].Path < todo[j].Path })
	return todo
}

// runReport is what one run did.
type runReport struct {
	kept, stayed        int
	jpegBytes, jxlBytes int64
	failed              map[string]string // why, by sha256, for the JPEGs that stay
	why                 map[string]int
}

// convert keeps each of todo as JPEG XL, jobs at a time, until ctx ends.
func convert(ctx context.Context, store *media.Store, todo []media.Item, jobs int, progress io.Writer) (*runReport, error) {
	r := &runReport{failed: map[string]string{}, why: map[string]int{}}
	var total int64
	for _, item := range todo {
		total += item.Sidecar.Facts.Size
	}
	fmt.Fprintf(progress, "%d JPEGs to keep as JPEG XL (%s)\n", len(todo), size(total))
	type result struct {
		item media.Item
		kept media.Item
		err  error
	}
	next, results := make(chan media.Item), make(chan result)
	var wg sync.WaitGroup
	for range min(jobs, max(1, len(todo))) {
		wg.Go(func() {
			for item := range next {
				kept, err := store.KeepAsJXL(item.Sha256())
				results <- result{item, kept, err}
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

	var errs []error
	started, lastProgress, done := time.Now(), time.Now(), 0
	for res := range results {
		done++
		switch {
		case res.err == nil:
			r.kept++
			r.jpegBytes += res.item.Sidecar.Facts.Size
			r.jxlBytes += res.kept.Sidecar.Facts.Size
		case errors.Is(res.err, media.ErrNotJPEG):
			// Changed since it was listed (kept by the server meanwhile).
		default:
			r.stayed++
			why := reason(res.err)
			r.why[why]++
			r.failed[res.item.Sha256()] = why
			if r.stayed <= 20 {
				log.Printf("%s stays a JPEG: %v", res.item.Path, res.err)
			}
			if !knownReason(res.err) {
				errs = append(errs, fmt.Errorf("%s: %w", res.item.Path, res.err))
			}
		}
		if time.Since(lastProgress) > progressEvery {
			lastProgress = time.Now()
			fmt.Fprintln(progress, r.progressLine(done, len(todo), time.Since(started)))
		}
	}
	if len(errs) > 5 {
		errs = append(errs[:5], fmt.Errorf("and %d more", len(errs)-5))
	}
	return r, errors.Join(errs...)
}

// progressEvery is how often a run says how far it is.
const progressEvery = 10 * time.Second

// progressLine is how far a run is after done of total files in elapsed, in plain ASCII for any terminal:
// "12.3% | 10612 of 86127 | 31.2 GB as JPEG -> 24.7 GB as JPEG XL (20.8% smaller, 6.49 GB saved) | 6.2/s, about 3h50m
// left".
func (r *runReport) progressLine(done, total int, elapsed time.Duration) string {
	rate := float64(done) / max(elapsed.Seconds(), 0.001)
	left := "?"
	if rate > 0 {
		left = (time.Duration(float64(total-done)/rate) * time.Second).Round(time.Minute).String()
	}
	return fmt.Sprintf("%.1f%% | %d of %d | %s as JPEG -> %s as JPEG XL (%s smaller, %s saved) | %.1f/s, about %s left",
		100*float64(done)/float64(max(total, 1)), done, total, size(r.jpegBytes), size(r.jxlBytes),
		percent(r.jpegBytes-r.jxlBytes, r.jpegBytes), size(r.jpegBytes-r.jxlBytes), rate, left)
}

// reason is why a JPEG stays, in a few words for the failure list and the report.
func reason(err error) string {
	if errors.Is(err, media.ErrNoSaving) {
		return "no saving"
	}
	msg := err.Error()
	if i := strings.IndexAny(msg, "\t\n"); i >= 0 {
		msg = msg[:i]
	}
	return msg
}

// knownReason reports whether a JPEG stays for a reason that is no fault of the run: libjxl can't keep it, or it
// wouldn't get smaller.
func knownReason(err error) bool {
	return errors.Is(err, media.ErrNoSaving) || strings.Contains(err.Error(), "libjxl") ||
		strings.Contains(err.Error(), "JPEG made again")
}

func (r *runReport) print(out io.Writer) {
	if r == nil {
		return
	}
	fmt.Fprintf(out, "this run: %d JPEGs kept as JPEG XL, %s as JPEG, %s as JPEG XL: %s saved (%s)\n", r.kept, size(r.jpegBytes),
		size(r.jxlBytes), size(r.jpegBytes-r.jxlBytes), percent(r.jpegBytes-r.jxlBytes, r.jpegBytes))
	if r.stayed > 0 {
		fmt.Fprintf(out, "%d stay JPEG:\n", r.stayed)
		whys := make([]string, 0, len(r.why))
		for why := range r.why {
			whys = append(whys, why)
		}
		sort.Slice(whys, func(i, j int) bool { return r.why[whys[i]] > r.why[whys[j]] })
		for _, why := range whys {
			fmt.Fprintf(out, "  %8d  %s\n", r.why[why], why)
		}
	}
}

// saved is what keeping JPEGs as JPEG XL has saved in a data folder, from its sidecars: the JPEG XLs against the
// JPEGs they were made from (encodedFrom), the JPEGs still kept as JPEG, and the whole library.
type saved struct {
	kept, jpegs           int
	keptJPEG, keptJXL     int64 // the JPEGs kept as JPEG XL, and those JPEG XLs
	jpegBytes, otherBytes int64 // JPEGs still JPEG; everything else (videos, HEIC, sound recordings)
}

func savings(store *media.Store) saved {
	var s saved
	store.Range(func(item media.Item) {
		f := item.Sidecar.Facts
		switch {
		case f.IsJXLOfJPEG():
			s.kept++
			s.keptJPEG += f.EncodedFrom.Size
			s.keptJXL += f.Size
		case f.Kind == media.KindPhoto && f.EncodedFrom == nil && media.IsJPEG(f.FileName, f.MimeType):
			s.jpegs++
			s.jpegBytes += f.Size
		default:
			s.otherBytes += f.Size
		}
	})
	return s
}

func (s saved) print(out io.Writer) {
	gain := s.keptJPEG - s.keptJXL
	fmt.Fprintf(out, "JPEGs kept as JPEG XL: %d, %s as JPEG, %s as JPEG XL: %s saved (%s of those JPEGs)\n", s.kept, size(s.keptJPEG),
		size(s.keptJXL), size(gain), percent(gain, s.keptJPEG))
	fmt.Fprintf(out, "still JPEG: %d (%s)\n", s.jpegs, size(s.jpegBytes))
	before := s.keptJPEG + s.jpegBytes + s.otherBytes
	fmt.Fprintf(out, "the whole data folder's originals: %s, %s before (%s smaller)\n", size(before-gain), size(before),
		percent(gain, before))
}

// estimate packs a sample of todo in memory, spread over it, and reports what keeping all of todo would save.
func estimate(ctx context.Context, store *media.Store, x *media.JXL, todo []media.Item, n, jobs int, out io.Writer) error {
	var total int64
	for _, item := range todo {
		total += item.Sidecar.Facts.Size
	}
	fmt.Fprintf(out, "%d JPEGs to keep as JPEG XL (%s); packing %d of them to estimate (dry run, nothing written)\n",
		len(todo), size(total), min(n, len(todo)))
	if len(todo) == 0 {
		return nil
	}
	var picked []media.Item
	step := max(1, len(todo)/n)
	for i := 0; i < len(todo) && len(picked) < n; i += step {
		picked = append(picked, todo[i])
	}
	var mu sync.Mutex
	var jpegBytes, jxlBytes int64
	var keptN, stayed int
	why := map[string]int{}
	next := make(chan media.Item)
	var wg sync.WaitGroup
	started := time.Now()
	for range min(jobs, len(picked)) {
		wg.Go(func() {
			for item := range next {
				data, err := os.ReadFile(filepath.Join(store.Root(), filepath.FromSlash(item.Path)))
				var jxl []byte
				if err == nil {
					jxl, err = x.Keep(data)
				}
				if err == nil && len(jxl) >= len(data) {
					err = media.ErrNoSaving
				}
				mu.Lock()
				if err != nil {
					stayed++
					why[reason(err)]++
				} else {
					keptN++
					jpegBytes += int64(len(data))
					jxlBytes += int64(len(jxl))
				}
				mu.Unlock()
			}
		})
	}
	for _, item := range picked {
		if ctx.Err() != nil {
			break
		}
		next <- item
	}
	close(next)
	wg.Wait()
	took := time.Since(started)
	if keptN == 0 {
		fmt.Fprintf(out, "none of the %d sampled JPEGs could be kept\n", stayed)
		return nil
	}
	ratio := float64(jxlBytes) / float64(jpegBytes)
	share := float64(keptN) / float64(keptN+stayed)
	gain := int64(float64(total) * share * (1 - ratio))
	fmt.Fprintf(out, "sample: %d kept, %s as JPEG, %s as JPEG XL (%s smaller); %d would stay JPEG\n", keptN, size(jpegBytes), size(jxlBytes),
		percent(jpegBytes-jxlBytes, jpegBytes), stayed)
	for w, c := range why {
		fmt.Fprintf(out, "  %8d  %s\n", c, w)
	}
	perFile := took / time.Duration(keptN+stayed)
	fmt.Fprintf(out, "estimate for all %d: about %s saved (%s of the JPEGs), taking about %s with --jobs %d\n", len(todo),
		size(gain), percent(gain, total), (perFile * time.Duration(len(todo))).Round(time.Minute), jobs)
	s := savings(store)
	before := s.keptJPEG + s.jpegBytes + s.otherBytes
	fmt.Fprintf(out, "the whole data folder's originals: %s now, about %s after (%s smaller)\n", size(before-(s.keptJPEG-s.keptJXL)),
		size(before-(s.keptJPEG-s.keptJXL)-gain), percent(gain, before-(s.keptJPEG-s.keptJXL)))
	return nil
}

// The JPEGs earlier runs couldn't keep: .cache/jxl/failed.tsv, "<sha256>\t<why>" a line, written in turns.
func failedPath(store *media.Store) string {
	return filepath.Join(store.Root(), ".cache", "jxl", "failed.tsv")
}

func readFailed(store *media.Store) map[string]string {
	failed := map[string]string{}
	f, err := os.Open(failedPath(store))
	if err != nil {
		return failed
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if sha, why, ok := strings.Cut(scanner.Text(), "\t"); ok {
			failed[sha] = why
		}
	}
	return failed
}

func writeFailed(store *media.Store, failed map[string]string) error {
	return store.Turn(func() error {
		path := failedPath(store)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		all := readFailed(store)
		for sha, why := range failed {
			all[sha] = why
		}
		shas := make([]string, 0, len(all))
		for sha := range all {
			shas = append(shas, sha)
		}
		sort.Strings(shas)
		var b strings.Builder
		for _, sha := range shas {
			b.WriteString(sha + "\t" + all[sha] + "\n")
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, path)
	})
}

// nextTime is the next time of day hh:mm after now, in local time.
func nextTime(hhmm string, now time.Time) (time.Time, error) {
	t, err := time.ParseInLocation("15:04", hhmm, now.Location())
	if err != nil {
		return time.Time{}, fmt.Errorf("%q isn't a time of day like 07:00", hhmm)
	}
	at := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
	if !at.After(now) {
		at = at.AddDate(0, 0, 1)
	}
	return at, nil
}

// size is bytes as people read them: "812 MB", "1.24 GB".
func size(n int64) string {
	switch a := float64(n); {
	case n < 0:
		return "-" + size(-n)
	case a >= 1<<40:
		return fmt.Sprintf("%.2f TB", a/(1<<40))
	case a >= 1<<30:
		return fmt.Sprintf("%.2f GB", a/(1<<30))
	case a >= 1<<20:
		return fmt.Sprintf("%.0f MB", a/(1<<20))
	case a >= 1<<10:
		return fmt.Sprintf("%.0f KB", a/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

func percent(part, whole int64) string {
	if whole == 0 {
		return "0%"
	}
	return fmt.Sprintf("%.1f%%", float64(part)*100/float64(whole))
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
