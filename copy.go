package main

import (
	"context"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"path/filepath"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/copies"
	"github.com/journal/server-go/media"
)

// newCopiers makes a copier for each copy in config.json of each user with a data folder (docs/copies.md), on a
// folder or a peer (docs/peers.md). A copy to a peer that isn't added, or isn't trusted, stops the server.
func newCopiers(config auth.AppConfig, library *media.Library) ([]*copies.Copier, error) {
	var copiers []*copies.Copier
	for _, user := range config.Users {
		store := library.Store(user.Username)
		if store == nil {
			continue
		}
		for _, cc := range user.Copies {
			var target copies.Target
			if cc.Peer != "" {
				var err error
				if target, err = peerTarget(config, user.Username, cc); err != nil {
					return nil, fmt.Errorf("user %s: %w", user.Username, err)
				}
			}
			copiers = append(copiers, copies.NewTo(user.Username, store, cc, target, time.Now))
		}
		// The user's own peers, added from their phone (swaps): their copy there.
		for _, key := range peerKeys(config) {
			info, _, err := loadPeer(config, key)
			if err != nil || info.User != user.Username || info.Copy == nil {
				continue
			}
			c, err := swapCopier(config, store, info)
			if err != nil {
				log.Printf("User %s's copy at %s: %v", user.Username, info.Name, err)
				continue
			}
			copiers = append(copiers, c)
		}
	}
	return copiers, nil
}

// swapCopier is the copy of info.User's data folder on their own peer.
func swapCopier(config auth.AppConfig, store *media.Store, info peerInfo) (*copies.Copier, error) {
	_, cert, err := loadPeer(config, info.key())
	if err != nil {
		return nil, err
	}
	var target copies.Target = peerFor(config, info, info.User, cert)
	if info.Copy.Encrypt {
		key, err := loadCopyKey(config, info.User)
		if err != nil {
			return nil, err
		}
		if target, err = copies.OpenEncrypted(target.(*copies.Peer), key,
			filepath.Join(peerDir(config, info.key()), "copies", info.User)); err != nil {
			return nil, err
		}
	} else if !info.Trusted {
		return nil, fmt.Errorf("peer %s isn't trusted and the copy isn't encrypted", info.Name)
	}
	cc := auth.CopyConfig{Peer: info.Name, Encrypt: info.Copy.Encrypt, At: info.Copy.At}
	if cc.At == "" {
		cc.At = auth.DefaultCopyAt
	}
	return copies.NewTo(info.User, store, cc, target, time.Now), nil
}

// copySet is the server's copies: config.json's and the users' swaps, which phones add and remove while it runs.
type copySet struct {
	ctx     context.Context
	mu      sync.Mutex
	list    []*copies.Copier
	cancels map[*copies.Copier]context.CancelFunc
	started bool // once start ran: a copy added then starts at once
}

func newCopySet(ctx context.Context, copiers []*copies.Copier) *copySet {
	return &copySet{ctx: ctx, list: copiers, cancels: map[*copies.Copier]context.CancelFunc{}}
}

func (s *copySet) all() []*copies.Copier {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.list)
}

// start keeps the copies up to date until the set's context ends: each one daily at its time, and the realtime ones
// as files arrive (and once now, for what arrived while the server was stopped). It warns about a copy on the data
// folder's disk or out of reach.
func (s *copySet) start(library *media.Library) {
	s.mu.Lock()
	s.started = true
	list := slices.Clone(s.list)
	s.mu.Unlock()
	for _, c := range list {
		s.run(c, false)
	}
	for _, user := range library.Users() {
		library.Store(user).OnWrite(func(items []media.Item) {
			for _, c := range s.all() {
				if c.User == user && c.Realtime {
					c.Changed(items)
				}
			}
		})
	}
}

// run starts c's passes until the set's context ends or c is removed; first runs a pass now.
func (s *copySet) run(c *copies.Copier, first bool) {
	ctx, cancel := context.WithCancel(s.ctx)
	s.mu.Lock()
	s.cancels[c] = cancel
	s.mu.Unlock()
	if same, err := c.SameDisk(); err == nil && same {
		log.Printf("Copy %s → %s is on the data folder's disk: a failing disk takes both", c.User, c.Folder)
	}
	if err := c.Ready(); err != nil {
		log.Printf("Copy %s → %s: %v", c.User, c.Folder, err)
	}
	mode := "daily at " + c.At
	if c.Realtime {
		mode += " and as files arrive"
		go c.RunRealtime(ctx)
	}
	if c.Realtime || first {
		go runCopy(ctx, c)
	}
	log.Printf("Copy %s → %s: %s", c.User, c.Folder, mode)
	go copyDaily(ctx, c)
}

// add keeps c too, and once the set is started runs a first pass of it now.
func (s *copySet) add(c *copies.Copier) {
	s.mu.Lock()
	s.list = append(s.list, c)
	started := s.started
	s.mu.Unlock()
	if started {
		s.run(c, true)
	}
}

// remove stops c (a pass going stops in the file in hand) and forgets it.
func (s *copySet) remove(c *copies.Copier) {
	s.mu.Lock()
	s.list = slices.DeleteFunc(s.list, func(x *copies.Copier) bool { return x == c })
	cancel := s.cancels[c]
	delete(s.cancels, c)
	s.mu.Unlock()
	c.Stop()
	if cancel != nil {
		cancel()
	}
}

// startCopies keeps copiers up to date until ctx ends (copySet.start).
func startCopies(ctx context.Context, library *media.Library, copiers []*copies.Copier) *copySet {
	set := newCopySet(ctx, copiers)
	set.start(library)
	return set
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

// runCopy runs a full pass and logs how it went, then compares the copy with the data folder for the apps (and the
// log, when files failed).
func runCopy(ctx context.Context, c *copies.Copier) {
	pass := c.Run(ctx)
	compared := c.CompareNow(ctx)
	took := pass.FinishedAt.Sub(pass.StartedAt).Round(time.Second)
	if pass.Err != nil {
		log.Printf("Copy %s → %s stopped after %s: %d files, %s copied, %d failed: %v", c.User, c.Folder, took,
			pass.Files, copies.Size(pass.Bytes), pass.Failed, pass.Err)
		return
	}
	if pass.Stopped {
		log.Printf("Copy %s → %s stopped from a phone after %s: %d files, %s copied, %d failed", c.User, c.Folder, took,
			pass.Files, copies.Size(pass.Bytes), pass.Failed)
		return
	}
	line := ""
	if pass.Failed > 0 && compared.Err == nil {
		line = ", not copied: " + notCopied(compared.Report)
	}
	if pass.Dropped > 0 {
		line += fmt.Sprintf(", %d JPEGs kept as JPEG XL deleted from the copy (%s)", pass.Dropped, copies.Size(pass.DroppedBytes))
	}
	log.Printf("Copy %s → %s: %d files, %s in %s, %d failed%s", c.User, c.Folder, pass.Files, copies.Size(pass.Bytes),
		took, pass.Failed, line)
}

func notCopied(r copies.Report) string {
	return fmt.Sprintf("%d originals (%s), %d sidecars", r.Originals, copies.Size(r.OriginalBytes), r.Sidecars)
}

// compareEvery is how old a copy's compare may be when an app asks how the copy is doing; an older one is made again.
const compareEvery = 10 * time.Minute

// copyRunner gives the public API each user's copies (resolver.CopyRunner); passes and compares it starts run until
// ctx ends.
type copyRunner struct {
	ctx context.Context
	set *copySet
}

func (r copyRunner) Of(user string) []*copies.Copier {
	var mine []*copies.Copier
	for _, c := range r.set.all() {
		if c.User == user {
			mine = append(mine, c)
		}
	}
	return mine
}

func (r copyRunner) Start(c *copies.Copier) {
	if running, _ := c.Status(); running == nil {
		go runCopy(r.ctx, c)
	}
}

func (r copyRunner) CompareIfOld(c *copies.Copier) { c.CompareIfOld(r.ctx, compareEvery) }
