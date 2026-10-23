// Package reading reads each file once for every index the server keeps of its users' photos and videos: the
// nudity scan, faces and text search today, and any added later (OCR, captions, …). An index says what it needs of a
// file (Wants); the reader decodes the original once, makes from it exactly the pictures each index asked for, and
// hands them to the indexes in their Order. Decoding the next files overlaps the models' work on this one. See
// docs/indexes.md.
package reading

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"image"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/journal/server-go/media"
)

// Index is one thing the server keeps about each file, made by reading it.
type Index interface {
	// Name names the index in logs and progress lines: "nudity", "faces", "search".
	Name() string
	// Order says who goes first when several need the same file; lower first. Hiding comes before finding.
	Order() int
	// Wants is what the index reads of a file. Asked once, when it is added.
	Wants() Wants
	// Needs reports whether user's item still has to be read by this index (by its present model).
	Needs(user string, item media.Item) bool
	// Read reads one file, given what Wants asked for, and keeps its result itself.
	Read(ctx context.Context, user string, in Input) error
	// Finish is called after a run of reads for user, whatever their outcome.
	Finish(user string)
	// FailureLog is where its unreadable files are listed: .cache/<folder>/<name> in the data folder.
	FailureLog() (folder, name string)
}

// Wants is what an index reads of a file.
type Wants struct {
	// The kinds of files it reads; a video is read as its poster frame.
	Kinds []media.Kind
	// The pictures it reads, each made from the original at exactly that size: Fit "contain" or media.SqueezeFit,
	// no crop. None: no decoding for it.
	Pictures []media.PreviewSpec
	// Regions: it cuts parts of the original itself (Input.Original), after seeing its pictures.
	Regions bool
}

// Input is one file as an index reads it.
type Input struct {
	Store *media.Store
	Item  media.Item
	// Exactly the pictures the index's Wants asked for.
	Pictures map[media.PreviewSpec]*image.RGBA
	// The decoded original, for Regions; nil unless the index wants them.
	Original Source
}

// Source is a file's decoded original (media.Original).
type Source interface {
	Picture(spec media.PreviewSpec) (*image.RGBA, error)
	Region(box media.Rect, width, height int) (*image.RGBA, error)
	Close()
}

// Opener decodes originals (media.Previews, through Previews).
type Opener interface {
	Open(ctx context.Context, store *media.Store, item media.Item, largest int) (Source, error)
}

// Previews opens originals with media.Previews.
type Previews struct{ *media.Previews }

func (p Previews) Open(ctx context.Context, store *media.Store, item media.Item, largest int) (Source, error) {
	o, err := p.Previews.Open(ctx, store, item, largest)
	if err != nil {
		return nil, err
	}
	return o, nil
}

// How often the users' stores are looked through for files not read yet (imports; uploads Wake the reader).
const rescanEvery = 30 * time.Second

// Reading many files logs its progress after each this many.
const progressEvery = 1000

// Reader reads the users' files for its indexes.
type Reader struct {
	library *media.Library
	opener  Opener
	prepare int
	indexes []Index
	wants   map[Index]Wants
	wake    chan struct{}
	// Files an index failed on, by index name: tried again only after a restart.
	failed map[string]map[string]bool
}

// New reads library's files with opener, preparing up to prepare files ahead of the indexes.
func New(library *media.Library, opener Opener, prepare int) *Reader {
	return &Reader{library: library, opener: opener, prepare: max(1, prepare), wants: map[Index]Wants{},
		wake: make(chan struct{}, 1), failed: map[string]map[string]bool{}}
}

// Add adds an index; before Run.
func (r *Reader) Add(x Index) {
	r.indexes = append(r.indexes, x)
	slices.SortStableFunc(r.indexes, func(a, b Index) int { return cmp.Compare(a.Order(), b.Order()) })
	r.wants[x] = x.Wants()
	r.failed[x.Name()] = map[string]bool{}
}

// Names is the indexes' names, in order.
func (r *Reader) Names() []string {
	names := make([]string, len(r.indexes))
	for i, x := range r.indexes {
		names[i] = x.Name()
	}
	return names
}

// Wake has Run look for new files now rather than at its next look: after an upload.
func (r *Reader) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run reads the files any index still needs, newest upload first, then looks for new ones after each upload (Wake)
// and every rescanEvery (imports), until ctx ends.
func (r *Reader) Run(ctx context.Context) {
	for {
		for _, user := range r.library.Users() {
			r.ReadNew(ctx, user)
		}
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-time.After(rescanEvery):
		}
	}
}

// file is one file to read and the indexes that need it.
type file struct {
	item    media.Item
	needing []Index
}

// prepared is a file with what its indexes asked for made, or why it couldn't be.
type prepared struct {
	file
	pictures map[media.PreviewSpec]*image.RGBA
	failed   map[media.PreviewSpec]error // pictures that couldn't be made
	err      error                       // the original couldn't be decoded
	source   Source                      // kept for Regions; closed after the last index
	release  func()                      // its memory, given back with it
}

// ReadNew reads user's files that any index needs, once each: stage 1 decodes the originals and makes the pictures
// (prepare files ahead, in goroutines of their own), stage 2 runs the indexes on them in order.
func (r *Reader) ReadNew(ctx context.Context, user string) {
	store := r.library.Store(user)
	if store == nil || len(r.indexes) == 0 {
		return
	}
	var todo []file
	store.Range(func(item media.Item) {
		f := file{item: item}
		for _, x := range r.indexes {
			if wantsKind(r.wants[x], item.Sidecar.Facts.Kind) && !r.failed[x.Name()][item.Sha256()] && x.Needs(user, item) {
				f.needing = append(f.needing, x)
			}
		}
		if len(f.needing) > 0 {
			todo = append(todo, f)
		}
	})
	if len(todo) == 0 {
		return
	}
	// Newest first: what was just uploaded is hidden and found soonest.
	sort.Slice(todo, func(i, j int) bool {
		return todo[i].item.Sidecar.Facts.UploadedAt.After(todo[j].item.Sidecar.Facts.UploadedAt)
	})
	if len(todo) > 1 {
		log.Printf("reading: %d files of %s for %s", len(todo), user, strings.Join(r.Names(), ", "))
	}

	// Stage 1: prepare files ahead, within the memory budget, into a queue of at most prepare files.
	b := newBudget(r.prepare+1, ctx)
	queue := make(chan file)
	ready := make(chan prepared, r.prepare)
	go func() {
		defer close(queue)
		for _, f := range todo {
			select {
			case queue <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for range r.prepare {
		wg.Go(func() {
			for f := range queue {
				ready <- r.prepareFile(ctx, store, f, b)
			}
		})
	}
	go func() { wg.Wait(); close(ready) }()

	// Stage 2: the indexes, in order, on each prepared file.
	stats := map[string]*count{}
	logs := map[string]*media.FailureLog{}
	for _, x := range r.indexes {
		stats[x.Name()] = &count{}
	}
	defer func() {
		for _, l := range logs {
			l.Close()
		}
	}()
	started, done := time.Now(), 0
	for p := range ready {
		for _, x := range p.needing {
			if ctx.Err() != nil {
				break
			}
			err := p.errFor(r.wants[x])
			if err == nil {
				err = x.Read(ctx, user, p.inputFor(store, r.wants[x]))
			}
			if err == nil {
				stats[x.Name()].read++
				continue
			}
			if ctx.Err() != nil {
				break
			}
			stats[x.Name()].failed++
			r.failed[x.Name()][p.item.Sha256()] = true
			if logs[x.Name()] == nil {
				folder, name := x.FailureLog()
				os.MkdirAll(filepath.Join(store.Root(), ".cache", folder), 0o755)
				logs[x.Name()] = media.NewFailureLog(store, x.Name(), folder, name)
			}
			logs[x.Name()].Add(p.item, err)
			// Without libvips or ffmpeg every file (or video) fails alike; the server said so at start.
			if !errors.Is(err, media.ErrPreviewsUnavailable) && !errors.Is(err, media.ErrPostersUnavailable) {
				log.Printf("reading: %s: %s: %v", x.Name(), p.item.Path, err)
			}
		}
		if p.source != nil {
			p.source.Close()
			p.release()
		}
		if done++; done%progressEvery == 0 {
			rate := float64(done) / time.Since(started).Seconds()
			left := time.Duration(float64(len(todo)-done)/rate) * time.Second
			log.Printf("reading: %d of %d files of %s (%s; %.1f/s, about %s left)", done, len(todo), user,
				r.summary(stats), rate, left.Round(time.Minute))
		}
	}
	for _, x := range r.indexes {
		x.Finish(user)
	}
	failed := 0
	for _, c := range stats {
		failed += c.failed
	}
	if len(todo) > 1 || failed > 0 {
		log.Printf("reading: read %d files of %s in %s: %s", done, user, time.Since(started).Round(time.Second), r.summary(stats))
	}
	for name, l := range logs {
		log.Printf("reading: the files %s could not read are listed in %s", name, l.Path)
	}
}

type count struct{ read, failed int }

// summary is each index's counts: "nudity 523, faces 520 (3 could not be read), search 0".
func (r *Reader) summary(stats map[string]*count) string {
	var parts []string
	for _, x := range r.indexes {
		c := stats[x.Name()]
		part := fmt.Sprintf("%s %d", x.Name(), c.read)
		if c.failed > 0 {
			part += fmt.Sprintf(" (%d could not be read)", c.failed)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

// prepareFile decodes f's original once and makes each picture its indexes want; nothing for indexes that want
// none. The original is kept (with its memory) only when an index cuts regions of it.
func (r *Reader) prepareFile(ctx context.Context, store *media.Store, f file, b *budget) prepared {
	p := prepared{file: f, pictures: map[media.PreviewSpec]*image.RGBA{}, failed: map[media.PreviewSpec]error{}}
	var specs []media.PreviewSpec
	regions := false
	for _, x := range f.needing {
		w := r.wants[x]
		regions = regions || w.Regions
		for _, s := range w.Pictures {
			if !slices.Contains(specs, s) {
				specs = append(specs, s)
			}
		}
	}
	if len(specs) == 0 && !regions {
		return p
	}
	largest := 0
	for _, s := range specs {
		largest = max(largest, s.Width, s.Height)
	}
	release, err := b.acquire(estimatedPixels(f.item, largest))
	if err != nil {
		p.err = err
		return p
	}
	source, err := r.opener.Open(ctx, store, f.item, largest)
	if err != nil {
		release()
		p.err = err
		return p
	}
	for _, s := range specs {
		if pic, err := source.Picture(s); err != nil {
			p.failed[s] = err
		} else {
			p.pictures[s] = pic
		}
	}
	if regions {
		p.source, p.release = source, release
	} else {
		source.Close()
		release()
	}
	return p
}

// errFor is why an index wanting w can't read p; nil when it can.
func (p prepared) errFor(w Wants) error {
	if p.err != nil {
		return p.err
	}
	for _, s := range w.Pictures {
		if err := p.failed[s]; err != nil {
			return err
		}
	}
	return nil
}

func (p prepared) inputFor(store *media.Store, w Wants) Input {
	in := Input{Store: store, Item: p.item, Pictures: map[media.PreviewSpec]*image.RGBA{}}
	for _, s := range w.Pictures {
		in.Pictures[s] = p.pictures[s]
	}
	if w.Regions {
		in.Original = p.source
	}
	return in
}

func wantsKind(w Wants, kind media.Kind) bool {
	return len(w.Kinds) == 0 || slices.Contains(w.Kinds, kind)
}

// Decoded originals are 3 bytes a pixel. Those over bigPixels are held one at a time; one of more than
// media's shrink limit is decoded at twice the largest side asked for.
const (
	bigPixels     = 50_000_000
	defaultPixels = 12_000_000 // a phone photo, when the sidecar has no size
	shrinkPixels  = 100_000_000
)

// estimatedPixels is about how many pixels item's decoded original will have.
func estimatedPixels(item media.Item, largest int) int {
	px := defaultPixels
	if w, h := item.Sidecar.Facts.Width, item.Sidecar.Facts.Height; w != nil && h != nil && *w > 0 && *h > 0 {
		px = *w * *h
	}
	if px > shrinkPixels && largest > 0 {
		px = 4 * largest * largest
	}
	return px
}

// budget bounds the decoded originals in memory: at most n at once, and only one of them big.
type budget struct {
	mu   sync.Mutex
	cond *sync.Cond
	n    int
	held int
	big  bool
	ctx  context.Context
}

func newBudget(n int, ctx context.Context) *budget {
	b := &budget{n: n, ctx: ctx}
	b.cond = sync.NewCond(&b.mu)
	go func() {
		<-ctx.Done()
		b.mu.Lock()
		b.cond.Broadcast()
		b.mu.Unlock()
	}()
	return b
}

// acquire waits for room for an original of pixels, and returns release, to give it back.
func (b *budget) acquire(pixels int) (release func(), err error) {
	big := pixels > bigPixels
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.ctx.Err() == nil && (b.held >= b.n || (big && b.big) || (big && b.held > 0) || b.big) {
		b.cond.Wait()
	}
	if err := b.ctx.Err(); err != nil {
		return nil, err
	}
	b.held++
	b.big = big
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.held--
			if big {
				b.big = false
			}
			b.cond.Broadcast()
			b.mu.Unlock()
		})
	}, nil
}
