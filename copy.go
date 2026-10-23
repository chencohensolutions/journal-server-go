package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/copies"
	"github.com/journal/server-go/media"
)

// newCopiers makes a copier for each copy in config.json of each user with a data folder (docs/copies.md).
func newCopiers(config auth.AppConfig, library *media.Library) []*copies.Copier {
	var copiers []*copies.Copier
	for _, user := range config.Users {
		store := library.Store(user.Username)
		if store == nil {
			continue
		}
		for _, cc := range user.Copies {
			copiers = append(copiers, copies.New(user.Username, store, cc, time.Now))
		}
	}
	return copiers
}

// startCopies keeps the copies up to date until ctx ends: each one daily at its time, and the realtime ones as
// files arrive (and once now, for what arrived while the server was stopped). It warns about a copy on the data
// folder's disk or out of reach.
func startCopies(ctx context.Context, library *media.Library, copiers []*copies.Copier) {
	byUser := map[string][]*copies.Copier{}
	for _, c := range copiers {
		if same, err := c.SameDisk(); err == nil && same {
			log.Printf("Copy %s → %s is on the data folder's disk: a failing disk takes both", c.User, c.Folder)
		}
		if err := c.Ready(); err != nil {
			log.Printf("Copy %s → %s: %v", c.User, c.Folder, err)
		}
		mode := "daily at " + c.At
		if c.Realtime {
			mode += " and as files arrive"
			byUser[c.User] = append(byUser[c.User], c)
			go c.RunRealtime(ctx)
			go runCopy(ctx, c)
		}
		log.Printf("Copy %s → %s: %s", c.User, c.Folder, mode)
		go copyDaily(ctx, c)
	}
	for user, realtime := range byUser {
		library.Store(user).OnWrite(func(items []media.Item) {
			for _, c := range realtime {
				c.Changed(items)
			}
		})
	}
}

// copyDaily runs a full pass at the copy's time every day until ctx ends.
func copyDaily(ctx context.Context, c *copies.Copier) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(nextRun(c.At, time.Now()))):
		}
		runCopy(ctx, c)
	}
}

// runCopy runs a full pass and logs how it went, with what the copy still lacks when it stopped short.
func runCopy(ctx context.Context, c *copies.Copier) {
	pass := c.Run(ctx)
	took := pass.FinishedAt.Sub(pass.StartedAt).Round(time.Second)
	if pass.Err != nil {
		log.Printf("Copy %s → %s stopped after %s: %d files, %s copied, %d failed: %v", c.User, c.Folder, took,
			pass.Files, copies.Size(pass.Bytes), pass.Failed, pass.Err)
		return
	}
	line := ""
	if pass.Failed > 0 {
		if report, err := c.Check(ctx, false); err == nil {
			line = ", not copied: " + notCopied(report)
		}
	}
	log.Printf("Copy %s → %s: %d files, %s in %s, %d failed%s", c.User, c.Folder, pass.Files, copies.Size(pass.Bytes),
		took, pass.Failed, line)
}

func notCopied(r copies.Report) string {
	return fmt.Sprintf("%d originals (%s), %d sidecars", r.Originals, copies.Size(r.OriginalBytes), r.Sidecars)
}
