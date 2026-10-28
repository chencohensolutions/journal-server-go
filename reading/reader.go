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
	// Name names the index in the code and its failure list: "nudity", "faces", "search".
	Name() string
	// Model is the model it reads with, as logs and progress lines name it: "nudenet-320n", "yunet-sface".
	Model() string
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

// Carrier is an index that can give a file the result it kept for another with the same pictures: a JPEG kept as
// JPEG XL (media.Facts.IsJXLOfJPEG) gets the JPEG's, instead of being read again.
type Carrier interface {
	// Carry keeps for to what was kept for from; false when nothing was.
	Carry(user, from, to string) (bool, error)
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

// Reading many files logs its progress about this often. The rate and the time left are counted from that last
// stretch alone, so a change of pace (videos, screenshots full of text) shows at once.
const progressEvery = time.Minute

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
	// Not call the indexes' Finish: in journal-server index, whose results the server puts together itself.
	skipFinish bool
	// OnProgress, when set, is told how a run is going after each file (and at its start and end): journal-server
	// index --progress writes it out for the server that started it. Set before the first run.
	OnProgress func(Progress)

	mu       sync.Mutex
	progress map[string]Progress           // the run in hand, by user
	last     map[string]LastRun            // how each user's last run ended
	paused   map[string]bool               // users whose reading waits for Resume
	cancels  map[string]context.CancelFunc // stops the run in hand, by user
}

// Progress is how far a run of one user's files has come.
type Progress struct {
	User      string       `json:"user"`
	Done      int          `json:"done"`
	Total     int          `json:"total"`
	StartedAt time.Time    `json:"startedAt"`
	Indexes   []IndexCount `json:"indexes"`
	// The run is over (read through, paused or stopped): its last report.
	Ended bool `json:"ended,omitempty"`
}

// IndexCount is how many files one index read in a run, and how many it couldn't.
type IndexCount struct {
	Name   string `json:"name"`
	Read   int    `json:"read"`
	Failed int    `json:"failed"`
}

// LastRun is how a run of one user's files ended.
type LastRun struct {
	StartedAt  time.Time
	FinishedAt time.Time
	Files      int
	Total      int // the files it set out to read
	Failed     int
	// It was paused or the server stopped before every file was read; the next run carries on.
	Stopped bool
}

// SkipFinish has ReadNew not call the indexes' Finish (faces grouping, duplicate finding): for a process that only
// writes the caches, while the server that answers queries does that work when it reads them.
func (r *Reader) SkipFinish() { r.skipFinish = true }

// New reads library's files with opener, preparing up to prepare files ahead of the indexes.
func New(library *media.Library, opener Opener, prepare int) *Reader {
	return &Reader{library: library, opener: opener, prepare: max(1, prepare), wants: map[Index]Wants{},
		wake: make(chan struct{}, 1), failed: map[string]map[string]bool{}, progress: map[string]Progress{},
		last: map[string]LastRun{}, paused: map[string]bool{}, cancels: map[string]context.CancelFunc{}}
}

// Progress is how far user's run in hand has come; false when none is.
func (r *Reader) Progress(user string) (Progress, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.progress[user]
	return p, ok
}

// LastRun is how user's last run (one that had files to read) ended; false before the first.
func (r *Reader) LastRun(user string) (LastRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.last[user]
	return l, ok
}

// Pause stops reading user's files: the run in hand finishes the file it is on, and Run skips user until Resume.
func (r *Reader) Pause(user string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paused[user] = true
	if cancel := r.cancels[user]; cancel != nil {
		cancel()
	}
}

// Resume reads user's files again, now.
func (r *Reader) Resume(user string) {
	r.mu.Lock()
	delete(r.paused, user)
	r.mu.Unlock()
	r.Wake()
}

// Paused reports whether user's reading waits for Resume.
func (r *Reader) Paused(user string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.paused[user]
}

// report keeps p as user's run in hand and tells OnProgress.
func (r *Reader) report(p Progress) {
	r.mu.Lock()
	r.progress[p.User] = p
	r.mu.Unlock()
	if r.OnProgress != nil {
		r.OnProgress(p)
	}
}

// counts is stats as IndexCounts, in the indexes' order.
func (r *Reader) counts(stats map[string]*count) []IndexCount {
	counts := make([]IndexCount, 0, len(r.indexes))
	for _, x := range r.indexes {
		c := stats[x.Name()]
		counts = append(counts, IndexCount{Name: x.Name(), Read: c.read, Failed: c.failed})
	}
	return counts
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

// Models is the indexes' models, in order.
func (r *Reader) Models() []string {
	models := make([]string, len(r.indexes))
	for i, x := range r.indexes {
		models[i] = x.Model()
	}
	return models
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
			if ctx.Err() != nil {
				return
			}
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

// carry gives each JPEG kept as JPEG XL what the indexes kept for its JPEG (Carrier), and returns the files still to
// read: those, for the indexes that kept nothing for the JPEG (or can't carry), and every other file.
func (r *Reader) carry(user string, todo []file) []file {
	carried := 0
	kept := todo[:0]
	for _, f := range todo {
		if facts := f.item.Sidecar.Facts; facts.IsJXLOfJPEG() {
			f.needing = slices.DeleteFunc(f.needing, func(x Index) bool {
				c, ok := x.(Carrier)
				if !ok {
					return false
				}
				done, err := c.Carry(user, facts.EncodedFrom.Sha256, f.item.Sha256())
				if err != nil {
					log.Printf("reading: %s: %s: giving it its JPEG's result: %v", x.Model(), f.item.Path, err)
				}
				return done && err == nil
			})
			if len(f.needing) == 0 {
				carried++
				continue
			}
		}
		kept = append(kept, f)
	}
	if carried > 0 {
		log.Printf("reading: %d JPEG XL files of %s got their JPEG's results, not read again", carried, user)
	}
	return kept
}

// ReadNew reads user's files that any index needs, once each: stage 1 decodes the originals and makes the pictures
// (prepare files ahead, in goroutines of their own), stage 2 runs the indexes on them in order. Nothing while user is
// paused; Pause stops it after the file in hand.
func (r *Reader) ReadNew(ctx context.Context, user string) {
	store := r.library.Store(user)
	if store == nil || len(r.indexes) == 0 || ctx.Err() != nil || r.Paused(user) {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.cancels[user] = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.cancels, user)
		r.mu.Unlock()
		cancel()
	}()
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
	todo = r.carry(user, todo)
	if len(todo) == 0 {
		return
	}
	// Newest first: what was just uploaded is hidden and found soonest.
	sort.Slice(todo, func(i, j int) bool {
		return todo[i].item.Sidecar.Facts.UploadedAt.After(todo[j].item.Sidecar.Facts.UploadedAt)
	})
	if len(todo) > 1 {
		log.Printf("reading: %d files of %s with %s", len(todo), user, strings.Join(r.Models(), ", "))
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
	lastAt, lastDone := started, 0
	r.report(Progress{User: user, Total: len(todo), StartedAt: started, Indexes: r.counts(stats)})
	defer func() {
		failed := 0
		for _, c := range stats {
			failed += c.failed
		}
		if r.OnProgress != nil {
			r.OnProgress(Progress{User: user, Done: done, Total: len(todo), StartedAt: started, Indexes: r.counts(stats), Ended: true})
		}
		r.mu.Lock()
		delete(r.progress, user)
		r.last[user] = LastRun{StartedAt: started, FinishedAt: time.Now(), Files: done, Total: len(todo),
			Failed: failed, Stopped: done < len(todo)}
		r.mu.Unlock()
	}()
	for p := range ready {
		if ctx.Err() != nil {
			// Stopped: what was prepared ahead is let go unread, and not counted.
			if p.source != nil {
				p.source.Close()
				p.release()
			}
			continue
		}
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
				logs[x.Name()] = media.NewFailureLog(store, x.Model(), folder, name)
			}
			logs[x.Name()].Add(p.item, err)
			// Without libvips or ffmpeg every file (or video) fails alike; the server said so at start.
			if !errors.Is(err, media.ErrPreviewsUnavailable) && !errors.Is(err, media.ErrPostersUnavailable) {
				log.Printf("reading: %s: %s: %v", x.Model(), p.item.Path, err)
			}
		}
		if p.source != nil {
			p.source.Close()
			p.release()
		}
		done++
		r.report(Progress{User: user, Done: done, Total: len(todo), StartedAt: started, Indexes: r.counts(stats)})
		if since := time.Since(lastAt); since >= progressEvery && done < len(todo) {
			rate := float64(done-lastDone) / since.Seconds()
			lastAt, lastDone = time.Now(), done
			left := time.Duration(float64(len(todo)-done)/rate) * time.Second
			log.Printf("reading: %d of %d files of %s (%s; %.1f/s, about %s left)", done, len(todo), user,
				r.summary(stats), rate, left.Round(time.Minute))
		}
	}
	if !r.skipFinish {
		for _, x := range r.indexes {
			x.Finish(user)
		}
	}
	// The nudity scan rewrote a sidecar for each file it read: save the start-up shortcut now, or the next start
	// reads those folders again from disk.
	if err := store.RefreshIndexCacheIfChanged(); err != nil {
		log.Printf("reading: saving the index cache of %s: %v", user, err)
	}
	failed := 0
	for _, c := range stats {
		failed += c.failed
	}
	if len(todo) > 1 || failed > 0 {
		log.Printf("reading: read %d files of %s in %s: %s", done, user, time.Since(started).Round(time.Second), r.summary(stats))
	}
	for _, x := range r.indexes {
		if l := logs[x.Name()]; l != nil {
			log.Printf("reading: the files %s could not read are listed in %s", x.Model(), l.Path)
		}
	}
}

type count struct{ read, failed int }

// summary is each index's counts, by model: "nudenet-320n 523, yunet-sface 520 (3 could not be read), …".
func (r *Reader) summary(stats map[string]*count) string {
	var parts []string
	for _, x := range r.indexes {
		c := stats[x.Name()]
		part := fmt.Sprintf("%s %d", x.Model(), c.read)
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
