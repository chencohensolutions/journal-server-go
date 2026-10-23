package main

import (
	"context"
	"errors"
	"fmt"
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
)

// runIndex is `journal-server index`: it reads the new files of every user for the indexes once, in this process,
// and exits, so their models take memory only while it runs. With "index": "daily" the server starts it at
// "index_at"; it can also be run by hand at any time. It holds each data folder's index lock while it runs, so it
// refuses to run beside another run, or beside a server that reads in itself ("index": "server"). When done it
// tells a running server (admin API, indexesUpdated) to read the results; Ctrl+C finishes the file in hand. With only
// (--only nudity,faces), just those indexes are loaded and read, so they can be brought up to date first.
func runIndex(config auth.AppConfig, only map[string]bool) error {
	started := time.Now()
	library, err := openLibrary(config)
	if err != nil {
		return err
	}
	var unlocks []func()
	defer func() {
		for _, unlock := range unlocks {
			unlock()
		}
	}()
	for _, user := range library.Users() {
		unlock, err := library.Store(user).LockIndexes()
		if errors.Is(err, media.ErrIndexesLocked) {
			return fmt.Errorf("%s: %w (a run still going, or a server with \"index\": \"server\")", user, err)
		}
		if err != nil {
			return err
		}
		unlocks = append(unlocks, unlock)
	}
	ix := loadIndexes(config, library, newPreviews(config), only)
	defer ix.close()
	if ix.reader == nil {
		return errors.New("no index has its model: nothing to read (go run ./scripts/bundle-onnx)")
	}
	ix.reader.SkipFinish()
	running, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for _, user := range library.Users() {
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

// parseOnly reads journal-server index's arguments: none for every index, or --only and a comma-separated list of
// index names (indexNames). nil means every index.
func parseOnly(args []string) (map[string]bool, error) {
	if len(args) == 0 {
		return nil, nil
	}
	if len(args) != 2 || args[0] != "--only" {
		return nil, fmt.Errorf("usage: journal-server index [--only %s]", strings.Join(indexNames, ","))
	}
	only := map[string]bool{}
	for _, name := range strings.Split(args[1], ",") {
		name = strings.TrimSpace(name)
		if !slices.Contains(indexNames, name) {
			return nil, fmt.Errorf("--only: no index %q (the indexes are %s)", name, strings.Join(indexNames, ", "))
		}
		only[name] = true
	}
	return only, nil
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

// indexDaily runs journal-server index at config.IndexAt every day as a separate process ("index": "daily"),
// copying its output into the server's log, and reads its results when it exits. Every 10 minutes it also reads
// what a run started by hand added. When ctx ends it stops a run in hand (which finishes the file it is reading,
// up to 30 s) and returns.
func indexDaily(ctx context.Context, config auth.AppConfig, ix indexes) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Minute):
				ix.refresh()
			}
		}
	}()
	for {
		next := nextRun(config.IndexAt, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
		runIndexProcess(ctx)
		ix.refresh()
	}
}

// runIndexProcess runs journal-server index, this same program, in the server's folder.
func runIndexProcess(ctx context.Context) {
	exe, err := os.Executable()
	if err != nil {
		log.Printf("Indexing: %v", err)
		return
	}
	cmd := exec.CommandContext(ctx, exe, "index")
	cmd.Stdout, cmd.Stderr = log.Writer(), log.Writer()
	// Stopping the server asks the run to stop (on Windows it shares the console and gets Ctrl+C itself); it
	// finishes the file it is reading, or is killed after 30 s.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 30 * time.Second
	log.Printf("Indexing: starting %s index", exe)
	started := time.Now()
	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		log.Printf("Indexing: the run failed after %s: %v", time.Since(started).Round(time.Second), err)
	}
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
