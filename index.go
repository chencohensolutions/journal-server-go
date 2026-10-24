package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/media"
	"github.com/journal/server-go/reading"
)

// runIndex is `journal-server index`: it reads the new files of every user (or of args.user alone) for the indexes
// once, in this process, and exits, so their models take memory only while it runs. With "index": "daily" the server
// starts it, one user at a time, at "index_at" and when a phone asks (indexrun.go); it can also be run by hand at any
// time. It holds each data folder's index lock while it runs, so it refuses to run beside another run of the same
// user, or beside a server that reads in itself ("index": "server"). When done it tells a running server (admin API,
// indexesUpdated) to read the results; Ctrl+C finishes the file in hand. With args.only (--only nudity,faces), just
// those indexes are loaded and read, so they can be brought up to date first. With args.progress, how the run is going
// is written to stdout as progress lines (writeProgress) for the server that started it; the log goes to stderr.
func runIndex(config auth.AppConfig, args indexArgs) error {
	started := time.Now()
	library, err := openLibrary(config)
	if err != nil {
		return err
	}
	users := library.Users()
	if args.user != "" {
		if library.Store(args.user) == nil {
			return fmt.Errorf("--user: no user %q in config.json", args.user)
		}
		users = []string{args.user}
	}
	var unlocks []func()
	defer func() {
		for _, unlock := range unlocks {
			unlock()
		}
	}()
	for _, user := range users {
		unlock, err := library.Store(user).LockIndexes()
		if errors.Is(err, media.ErrIndexesLocked) {
			return fmt.Errorf("%s: %w (a run still going, or a server with \"index\": \"server\")", user, err)
		}
		if err != nil {
			return err
		}
		unlocks = append(unlocks, unlock)
	}
	ix := loadIndexes(config, library, newPreviews(config), args.only)
	defer ix.close()
	if ix.reader == nil {
		return errors.New("no index has its model: nothing to read (go run ./scripts/bundle-onnx)")
	}
	ix.reader.SkipFinish()
	if args.progress {
		ix.reader.OnProgress = writeProgress(os.Stdout, time.Second)
	}
	running, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for _, user := range users {
		ix.reader.ReadNew(running, user)
	}
	library.RefreshIndexCachesIfChanged()
	if running.Err() != nil {
		log.Printf("index: stopped after %s; the next run carries on", time.Since(started).Round(time.Second))
	} else {
		log.Printf("index: done in %s", time.Since(started).Round(time.Second))
	}
	notifyServer(config)
	return nil
}

// indexArgs is what journal-server index was asked.
type indexArgs struct {
	only     map[string]bool // nil: every index
	user     string          // "": every user
	progress bool
}

// parseIndexArgs reads journal-server index's arguments: --only and a comma-separated list of index names
// (indexNames; none for every index), --user and a user name (none for every user), and --progress.
func parseIndexArgs(args []string) (indexArgs, error) {
	var a indexArgs
	usage := fmt.Errorf("usage: journal-server index [--only %s] [--user name] [--progress]", strings.Join(indexNames, ","))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--progress":
			a.progress = true
		case "--user":
			if i+1 == len(args) || a.user != "" {
				return indexArgs{}, usage
			}
			i++
			a.user = args[i]
		case "--only":
			if i+1 == len(args) || a.only != nil {
				return indexArgs{}, usage
			}
			i++
			a.only = map[string]bool{}
			for _, name := range strings.Split(args[i], ",") {
				name = strings.TrimSpace(name)
				if !slices.Contains(indexNames, name) {
					return indexArgs{}, fmt.Errorf("--only: no index %q (the indexes are %s)", name, strings.Join(indexNames, ", "))
				}
				a.only[name] = true
			}
		default:
			return indexArgs{}, usage
		}
	}
	return a, nil
}

// progressPrefix starts each progress line journal-server index --progress writes: the prefix, then the
// reading.Progress as JSON.
const progressPrefix = "@progress "

// writeProgress writes a progress line to w for a run's start and end (read through or stopped) and at most once
// every each between.
func writeProgress(w io.Writer, each time.Duration) func(reading.Progress) {
	var last time.Time
	return func(p reading.Progress) {
		if p.Done != 0 && !p.Ended && time.Since(last) < each {
			return
		}
		last = time.Now()
		line, _ := json.Marshal(p)
		fmt.Fprintf(w, "%s%s\n", progressPrefix, line)
	}
}

// progressWriter takes journal-server index's stdout: each progress line goes to onProgress, anything else to the
// log.
type progressWriter struct {
	onProgress func(reading.Progress)
	partial    []byte
}

func (pw *progressWriter) Write(b []byte) (int, error) {
	pw.partial = append(pw.partial, b...)
	for {
		i := bytes.IndexByte(pw.partial, '\n')
		if i < 0 {
			return len(b), nil
		}
		line := string(pw.partial[:i])
		pw.partial = pw.partial[i+1:]
		var p reading.Progress
		if rest, ok := strings.CutPrefix(line, progressPrefix); ok && json.Unmarshal([]byte(rest), &p) == nil {
			if pw.onProgress != nil {
				pw.onProgress(p)
			}
			continue
		}
		log.Print(line)
	}
}

// notifyServer tells a running server that the indexes' caches changed, so it reads them now; no server, no harm.
func notifyServer(config auth.AppConfig) {
	var data struct {
		IndexesUpdated bool `json:"indexesUpdated"`
	}
	if err := adminQuery(config, `mutation { indexesUpdated }`, nil, &data); err != nil {
		log.Printf("index: the server wasn't told (%v); it reads the results within 10 minutes", err)
		return
	}
	if data.IndexesUpdated {
		log.Printf("index: the server read the results")
	}
}

// runIndexProcess runs journal-server index with args, this same program, in the server's folder, until it exits or
// ctx ends. Its progress lines go to onProgress, and the rest of its output into the server's log. When ctx ends it
// is asked to stop (on Windows it shares the console and gets Ctrl+C itself): it finishes the file it is reading,
// or is killed after 30 s.
func runIndexProcess(ctx context.Context, onProgress func(reading.Progress), args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, exe, append([]string{"index"}, args...)...)
	cmd.Stdout, cmd.Stderr = &progressWriter{onProgress: onProgress}, log.Writer()
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 30 * time.Second
	log.Printf("Indexing: starting %s index %s", exe, strings.Join(args, " "))
	started := time.Now()
	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		log.Printf("Indexing: the run failed after %s: %v", time.Since(started).Round(time.Second), err)
		return err
	}
	return nil
}

// nextRun is the next time at ("15:04", local time) after now.
func nextRun(at string, now time.Time) time.Time {
	t, err := time.Parse("15:04", at)
	if err != nil {
		t, _ = time.Parse("15:04", "03:00")
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// lockIndexes takes every data folder's index lock, for a server that reads files itself ("index": "server"), so
// journal-server index can't run beside it.
func lockIndexes(library *media.Library) error {
	for _, user := range library.Users() {
		if _, err := library.Store(user).LockIndexes(); err != nil {
			return fmt.Errorf("%s: %w", user, err)
		}
	}
	return nil
}
