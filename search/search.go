// Package search finds a user's photos and videos by what they show, from a few words in any language ("dog on the
// beach", "כלב בים"). A SigLIP 2 model, run in Python as a separate program (siglip_worker.py), gives each photo a
// vector, and each query: the photos whose vectors point most nearly the same way as the query's come first.
//
// The server reads every photo once, in the background, newest first, from its 640 px preview (a video from its
// poster), and new files as they are uploaded, or within a minute of their import. The vectors are kept per user in
// .cache/search/ and in memory; a search compares the query with all of them. See docs/media.md.
package search

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/journal/server-go/media"
)

// Pictures makes the picture the model reads of a file, and release, to call once it has been read.
type Pictures func(ctx context.Context, store *media.Store, item media.Item) (path string, release func(), err error)

// FromPreviews reads files from their previews (media.ModelPreview), never the originals.
func FromPreviews(previews *media.Previews) Pictures {
	return func(ctx context.Context, store *media.Store, item media.Item) (string, func(), error) {
		return previews.Get(ctx, store, item, media.ModelPreview)
	}
}

const (
	// Pictures read together: faster than one by one, and short enough that a query waits little behind them.
	batchSize = 8
	// How often the stores are looked at for files not read yet.
	lookEvery = 30 * time.Second
	// How long to wait before starting the model again after it failed to start.
	restartAfter = time.Minute
	// The longest query read, in bytes; the model reads 64 tokens at most anyway.
	MaxQueryBytes = 1000
)

// ErrNotReady is a search before the model has loaded, or after it stopped.
var ErrNotReady = errors.New("search is starting (loading the model); try again in a minute")

// Engine reads every user's files with the model, and answers searches.
type Engine struct {
	library  *media.Library
	model    string
	command  []string
	pictures Pictures
	wake     chan struct{}

	mu      sync.Mutex
	worker  *worker // nil until the model has loaded
	why     error   // why it hasn't, when it failed
	indexes map[string]*index
}

// index is one user's vectors, and the files that couldn't be read since the server started (tried again on the
// next start).
type index struct {
	store   *media.Store
	vectors *vectors
	failed  map[string]bool
}

// New is an engine for library's users, reading pictures with model through command (see PythonCommand). It does
// nothing until Run.
func New(library *media.Library, model string, command []string, pictures Pictures) *Engine {
	return &Engine{library: library, model: model, command: command, pictures: pictures, wake: make(chan struct{}, 1)}
}

// Wake makes Run look for new files now rather than at its next look: after an upload.
func (e *Engine) Wake() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Run starts the model, then reads the files not read yet, and keeps doing so, until ctx is done. A model that
// stops is started again.
func (e *Engine) Run(ctx context.Context) {
	for ctx.Err() == nil {
		log.Printf("search: loading %s (the first start downloads it)", e.model)
		w, err := startWorker(e.command)
		if err == nil {
			err = e.ready(w)
			if err != nil {
				w.close()
			}
		}
		if err != nil {
			log.Printf("search: %v; trying again in %s", err, restartAfter)
			e.setWorker(nil, err)
			select {
			case <-ctx.Done():
			case <-time.After(restartAfter):
			}
			continue
		}
		log.Printf("search: %s loaded (%d dimensions, on %s)", e.model, w.dims, w.device)
		for ctx.Err() == nil && !w.isBroken() {
			e.readAll(ctx, w)
			select {
			case <-ctx.Done():
			case <-e.wake:
			case <-time.After(lookEvery):
			}
		}
		e.setWorker(nil, nil)
		w.close()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, idx := range e.indexes {
		idx.vectors.close()
	}
	e.indexes = nil
}

// ready opens every user's vectors for w's size (once; again if a new model has another size) and puts w to work.
func (e *Engine) ready(w *worker) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for user, idx := range e.indexes {
		if idx.vectors.dims == w.dims {
			continue
		}
		idx.vectors.close()
		delete(e.indexes, user)
	}
	if e.indexes == nil {
		e.indexes = map[string]*index{}
	}
	for _, user := range e.library.Users() {
		if e.indexes[user] != nil {
			continue
		}
		store := e.library.Store(user)
		v, err := openVectors(store.Root(), e.model, w.dims)
		if err != nil {
			return fmt.Errorf("%s: %w", user, err)
		}
		e.indexes[user] = &index{store: store, vectors: v, failed: map[string]bool{}}
	}
	e.worker, e.why = w, nil
	return nil
}

func (e *Engine) setWorker(w *worker, why error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.worker, e.why = w, why
}

// readAll reads, for every user, the files not read yet.
func (e *Engine) readAll(ctx context.Context, w *worker) {
	e.mu.Lock()
	var indexes []*index
	var users []string
	for user, idx := range e.indexes {
		users = append(users, user)
		indexes = append(indexes, idx)
	}
	e.mu.Unlock()
	for i, idx := range indexes {
		if err := e.read(ctx, w, users[i], idx); err != nil {
			if ctx.Err() == nil {
				log.Printf("search: %s: %v", users[i], err)
			}
			return
		}
	}
}

// read reads one user's files not read yet, newest first. It stops at the first error of the model itself; a file
// it can't make a picture of, or the model can't read, is left out until the server starts again.
func (e *Engine) read(ctx context.Context, w *worker, user string, idx *index) error {
	var todo []media.Item
	idx.store.Range(func(item media.Item) {
		if !idx.failed[item.Sha256()] && !idx.vectors.has(item.Sha256()) {
			todo = append(todo, item)
		}
	})
	if len(todo) == 0 {
		return nil
	}
	slices.SortFunc(todo, func(a, b media.Item) int {
		return b.Sidecar.Facts.UploadedAt.Compare(a.Sidecar.Facts.UploadedAt)
	})
	if len(todo) > batchSize {
		log.Printf("search: %s: reading %d files", user, len(todo))
	}
	started, lastLog := time.Now(), time.Now()
	done, failed := 0, 0
	var firstFailure string
	for batch := range slices.Chunk(todo, batchSize) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		paths, releases, errs := e.makePictures(ctx, idx.store, batch)
		var ready []int
		var readyPaths []string
		for i := range batch {
			if errs[i] == nil {
				ready = append(ready, i)
				readyPaths = append(readyPaths, paths[i])
			}
		}
		var vecs [][]float32
		var err error
		if len(readyPaths) > 0 {
			var readErrs []error
			vecs, readErrs, err = readPictures(w, readyPaths)
			for j, i := range ready {
				if err == nil {
					errs[i] = readErrs[j]
				}
			}
		}
		for _, release := range releases {
			release()
		}
		if err != nil {
			return err
		}
		var shas []string
		var good [][]float32
		for j, i := range ready {
			if errs[i] == nil {
				shas = append(shas, batch[i].Sha256())
				good = append(good, vecs[j])
			}
		}
		if err := idx.vectors.add(shas, good); err != nil {
			return err
		}
		for i, item := range batch {
			if errs[i] != nil && ctx.Err() == nil {
				idx.failed[item.Sha256()] = true
				failed++
				if firstFailure == "" {
					firstFailure = fmt.Sprintf("%s: %v", item.Path, errs[i])
				}
			}
		}
		done += len(batch)
		if time.Since(lastLog) > time.Minute {
			lastLog = time.Now()
			log.Printf("search: %s: %d of %d read (%.1f/s)", user, done, len(todo), float64(done)/time.Since(started).Seconds())
		}
	}
	if len(todo) > batchSize {
		log.Printf("search: %s: read %d files in %s", user, done-failed, time.Since(started).Round(time.Second))
	}
	if failed > 0 {
		log.Printf("search: %s: %d files couldn't be read, such as %s", user, failed, firstFailure)
	}
	return nil
}

// readPictures reads a batch with the model. When the model fails the batch as a whole (one picture it chokes on),
// the pictures are read one by one, so only that one is left out. err is the model itself failing.
func readPictures(w *worker, paths []string) ([][]float32, []error, error) {
	vecs, errs, err := w.images(paths)
	if err == nil || w.isBroken() {
		return vecs, errs, err
	}
	vecs, errs = make([][]float32, len(paths)), make([]error, len(paths))
	if len(paths) == 1 {
		errs[0] = err
		return vecs, errs, nil
	}
	for i, path := range paths {
		one, oneErrs, err := readPictures(w, []string{path})
		if err != nil {
			return nil, nil, err
		}
		vecs[i], errs[i] = one[0], oneErrs[0]
	}
	return vecs, errs, nil
}

// makePictures makes the pictures of a batch at once (libvips runs as many as it has room for).
func (e *Engine) makePictures(ctx context.Context, store *media.Store, batch []media.Item) (paths []string, releases []func(), errs []error) {
	paths, releases, errs = make([]string, len(batch)), make([]func(), len(batch)), make([]error, len(batch))
	var wg sync.WaitGroup
	for i, item := range batch {
		wg.Go(func() {
			paths[i], releases[i], errs[i] = e.pictures(ctx, store, item)
			if releases[i] == nil {
				releases[i] = func() {}
			}
		})
	}
	wg.Wait()
	return paths, releases, errs
}

// Hit is a file found, with how close it is to the query: about -1 to 1, only for ordering within one search.
type Hit struct {
	Item  media.Item
	Score float64
}

// Result is a search's best files, best first, and how many of the files searched have been read so far.
type Result struct {
	Hits           []Hit
	Indexed, Total int
}

// Search finds user's files that best match query, at most limit of them. Files hide reports true for are left
// out, and not counted.
func (e *Engine) Search(user, query string, limit int, hide func(media.Item) bool) (Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return Result{}, errors.New("the query is empty")
	}
	if len(query) > MaxQueryBytes {
		return Result{}, fmt.Errorf("the query is longer than %d bytes", MaxQueryBytes)
	}
	e.mu.Lock()
	w, why, idx := e.worker, e.why, e.indexes[user]
	e.mu.Unlock()
	if w == nil {
		if why != nil {
			return Result{}, fmt.Errorf("search is off for now: %w", why)
		}
		return Result{}, ErrNotReady
	}
	if idx == nil {
		return Result{}, fmt.Errorf("%s has no data folder", user)
	}
	vec, err := w.text(query)
	if err != nil {
		return Result{}, err
	}

	var result Result
	var shas []string
	idx.store.Range(func(item media.Item) {
		if hide != nil && hide(item) {
			return
		}
		result.Total++
		shas = append(shas, item.Sha256())
	})
	scores := idx.vectors.scores(vec, shas)
	type scored struct {
		sha   string
		score float32
	}
	found := make([]scored, 0, len(shas))
	for i, sha := range shas {
		if !math.IsNaN(float64(scores[i])) {
			found = append(found, scored{sha, scores[i]})
		}
	}
	result.Indexed = len(found)
	slices.SortFunc(found, func(a, b scored) int { return cmp.Or(cmp.Compare(b.score, a.score), strings.Compare(a.sha, b.sha)) })
	for _, f := range found[:min(limit, len(found))] {
		if item, ok := idx.store.Get(f.sha); ok {
			result.Hits = append(result.Hits, Hit{Item: item, Score: float64(f.score)})
		}
	}
	return result, nil
}
