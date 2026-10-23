package reading

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/journal/server-go/media"
)

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

var (
	small  = media.PreviewSpec{Width: 320, Height: 320, Fit: "contain"}
	medium = media.PreviewSpec{Width: 640, Height: 640, Fit: "contain"}
	large  = media.PreviewSpec{Width: 1280, Height: 1280, Fit: "contain"}
	square = media.PreviewSpec{Width: 256, Height: 256, Fit: media.SqueezeFit}
)

// library is a data folder of hen with a file per kind given (its content made up), and their sha256s.
func library(t *testing.T, kinds ...media.Kind) (*media.Library, []string) {
	t.Helper()
	l, err := media.OpenLibrary(map[string]string{"hen": t.TempDir()}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var shas []string
	for _, kind := range kinds {
		shas = append(shas, addFile(t, l, kind, nil))
	}
	return l, shas
}

var files atomic.Int64

// addFile adds a made-up file of kind (of pixels, when given) to hen's folder.
func addFile(t *testing.T, l *media.Library, kind media.Kind, pixels *int) string {
	t.Helper()
	n := files.Add(1)
	data := []byte(fmt.Sprintf("file %d", n))
	path := filepath.Join(t.TempDir(), "f")
	os.WriteFile(path, data, 0o644)
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	req := media.UploadRequest{Sha256: sha, Size: int64(len(data)), FileName: fmt.Sprintf("F_%d.jpg", n),
		MimeType: "image/jpeg", Kind: kind, UploadedBy: "test"}
	if kind == media.KindVideo {
		req.FileName, req.MimeType = fmt.Sprintf("F_%d.mp4", n), "video/mp4"
	}
	if pixels != nil {
		w, h := *pixels/1000, 1000
		req.Width, req.Height = &w, &h
	}
	if _, _, err := l.Store("hen").Import(path, req); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // distinct upload times: newest first is a fixed order
	return sha
}

// opener is a fake decoder: it counts what it is asked, can be slow or fail, and tracks how many originals are
// open at once.
type opener struct {
	delay  time.Duration
	fail   map[string]bool
	mu     sync.Mutex
	opened map[string]int
	made   map[string][]media.PreviewSpec
	open   int
	most   int
	closed int
}

func newOpener() *opener {
	return &opener{fail: map[string]bool{}, opened: map[string]int{}, made: map[string][]media.PreviewSpec{}}
}

func (o *opener) Open(ctx context.Context, store *media.Store, item media.Item, largest int) (Source, error) {
	time.Sleep(o.delay)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.opened[item.Sha256()]++
	if o.fail[item.Sha256()] {
		return nil, errors.New("can't decode")
	}
	o.open++
	o.most = max(o.most, o.open)
	return &source{o: o, sha: item.Sha256()}, nil
}

type source struct {
	o      *opener
	sha    string
	closed bool
}

func (s *source) Picture(spec media.PreviewSpec) (*image.RGBA, error) {
	s.o.mu.Lock()
	defer s.o.mu.Unlock()
	s.o.made[s.sha] = append(s.o.made[s.sha], spec)
	return image.NewRGBA(image.Rect(0, 0, spec.Width, spec.Height)), nil
}

func (s *source) Region(box media.Rect, w, h int) (*image.RGBA, error) {
	return image.NewRGBA(image.Rect(0, 0, w, h)), nil
}

func (s *source) Close() {
	s.o.mu.Lock()
	defer s.o.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.o.open--
		s.o.closed++
	}
}

// index is a fake index: it has read the files in done, records each read in log, and can be slow or fail.
type index struct {
	name     string
	order    int
	wants    Wants
	delay    time.Duration
	fail     map[string]bool
	mu       sync.Mutex
	done     map[string]bool
	reads    []Input
	log      *[]string
	logMu    *sync.Mutex
	finished int
}

func newIndex(name string, order int, wants Wants, log *[]string, logMu *sync.Mutex) *index {
	return &index{name: name, order: order, wants: wants, fail: map[string]bool{}, done: map[string]bool{}, log: log, logMu: logMu}
}

func (x *index) Name() string  { return x.name }
func (x *index) Model() string { return x.name + "-model" }
func (x *index) Order() int    { return x.order }
func (x *index) Wants() Wants  { return x.wants }
func (x *index) Needs(user string, item media.Item) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return !x.done[item.Sha256()]
}
func (x *index) Read(ctx context.Context, user string, in Input) error {
	time.Sleep(x.delay)
	x.logMu.Lock()
	*x.log = append(*x.log, x.name+":"+in.Item.Sha256()[:8])
	x.logMu.Unlock()
	x.mu.Lock()
	defer x.mu.Unlock()
	x.reads = append(x.reads, in)
	if x.fail[in.Item.Sha256()] {
		return errors.New("the model can't")
	}
	x.done[in.Item.Sha256()] = true
	return nil
}
func (x *index) Finish(string)                     { x.finished++ }
func (x *index) FailureLog() (folder, name string) { return x.name, "test.failed.log" }

func photos() []media.Kind { return []media.Kind{media.KindPhoto} }

func TestEachFileIsDecodedOnceForEveryIndex(t *testing.T) {
	lib, shas := library(t, media.KindPhoto, media.KindPhoto, media.KindPhoto)
	var calls []string
	var mu sync.Mutex
	nudity := newIndex("nudity", 0, Wants{Kinds: photos(), Pictures: []media.PreviewSpec{small}}, &calls, &mu)
	faces := newIndex("faces", 10, Wants{Kinds: photos(), Pictures: []media.PreviewSpec{medium, large}, Regions: true}, &calls, &mu)
	search := newIndex("search", 20, Wants{Kinds: photos(), Pictures: []media.PreviewSpec{square}}, &calls, &mu)
	o := newOpener()
	r := New(lib, o, 2)
	for _, x := range []*index{search, nudity, faces} { // added in any order
		r.Add(x)
	}
	r.ReadNew(context.Background(), "hen")

	for _, sha := range shas {
		if o.opened[sha] != 1 {
			t.Errorf("%s decoded %d times", sha[:8], o.opened[sha])
		}
		made := slices.Clone(o.made[sha])
		if len(made) != 4 {
			t.Errorf("%s: pictures made %v, want each of the 4 sizes once", sha[:8], made)
		}
		// Each index, in order, on each file.
		var order []string
		for _, c := range calls {
			if strings.HasSuffix(c, sha[:8]) {
				order = append(order, strings.Split(c, ":")[0])
			}
		}
		if !slices.Equal(order, []string{"nudity", "faces", "search"}) {
			t.Errorf("%s read by %v", sha[:8], order)
		}
	}
	check := func(x *index, specs ...media.PreviewSpec) {
		t.Helper()
		if len(x.reads) != 3 {
			t.Fatalf("%s read %d files", x.name, len(x.reads))
		}
		for _, in := range x.reads {
			if len(in.Pictures) != len(specs) {
				t.Errorf("%s got %d pictures", x.name, len(in.Pictures))
			}
			for _, s := range specs {
				if p := in.Pictures[s]; p == nil || p.Bounds().Dx() != s.Width {
					t.Errorf("%s: no %v", x.name, s)
				}
			}
			if (in.Original != nil) != x.wants.Regions {
				t.Errorf("%s: original %v", x.name, in.Original)
			}
		}
		if x.finished != 1 {
			t.Errorf("%s finished %d times", x.name, x.finished)
		}
	}
	check(nudity, small)
	check(faces, medium, large)
	check(search, square)
	if o.open != 0 || o.closed != 3 {
		t.Errorf("%d originals still open, %d closed", o.open, o.closed)
	}

	// Nothing left: nothing decoded again.
	r.ReadNew(context.Background(), "hen")
	if o.opened[shas[0]] != 1 {
		t.Error("read again with nothing to do")
	}
}

// Only what is still needed is made: the sizes of the indexes that still need a file, and nothing for an index
// that reads no picture. A photo-only index never gets a video.
func TestOnlyWhatIsNeededIsMade(t *testing.T) {
	lib, shas := library(t, media.KindPhoto, media.KindVideo)
	var calls []string
	var mu sync.Mutex
	nudity := newIndex("nudity", 0, Wants{Kinds: []media.Kind{media.KindPhoto, media.KindVideo}, Pictures: []media.PreviewSpec{small}}, &calls, &mu)
	search := newIndex("search", 20, Wants{Kinds: photos(), Pictures: []media.PreviewSpec{square}}, &calls, &mu)
	exif := newIndex("exif", 30, Wants{}, &calls, &mu) // reads the sidecar: no picture
	nudity.done[shas[0]] = true
	o := newOpener()
	r := New(lib, o, 2)
	r.Add(nudity)
	r.Add(search)
	r.Add(exif)
	r.ReadNew(context.Background(), "hen")

	if made := o.made[shas[0]]; !slices.Equal(made, []media.PreviewSpec{square}) {
		t.Errorf("photo: made %v, want only search's", made)
	}
	if made := o.made[shas[1]]; !slices.Equal(made, []media.PreviewSpec{small}) {
		t.Errorf("video: made %v, want only nudity's", made)
	}
	for _, in := range search.reads {
		if in.Item.Sidecar.Facts.Kind == media.KindVideo {
			t.Error("search, for photos, was given a video")
		}
	}
	if len(exif.reads) != 2 || len(exif.reads[0].Pictures) != 0 {
		t.Errorf("exif read %d files", len(exif.reads))
	}
	// A file no needing index wants a picture of is not decoded.
	exifOnly := addFile(t, lib, media.KindPhoto, nil)
	nudity.done[exifOnly], search.done[exifOnly] = true, true
	r.ReadNew(context.Background(), "hen")
	if o.opened[exifOnly] != 0 || !exif.done[exifOnly] {
		t.Errorf("decoded %d times for an index that reads no picture", o.opened[exifOnly])
	}
}

// A file that can't be decoded fails every index that needed it; a model error fails only its index. Each lists
// its failures, and none is tried again until a restart.
func TestFailuresAreKeptPerIndex(t *testing.T) {
	lib, shas := library(t, media.KindPhoto, media.KindPhoto, media.KindPhoto)
	var calls []string
	var mu sync.Mutex
	nudity := newIndex("nudity", 0, Wants{Kinds: photos(), Pictures: []media.PreviewSpec{small}}, &calls, &mu)
	faces := newIndex("faces", 10, Wants{Kinds: photos(), Pictures: []media.PreviewSpec{large}}, &calls, &mu)
	o := newOpener()
	o.fail[shas[0]] = true
	faces.fail[shas[1]] = true
	r := New(lib, o, 2)
	r.Add(nudity)
	r.Add(faces)
	r.ReadNew(context.Background(), "hen")
	r.ReadNew(context.Background(), "hen")

	root := lib.Store("hen").Root()
	listed := func(name string) []string {
		data, _ := os.ReadFile(filepath.Join(root, ".cache", name, "test.failed.log"))
		var shas []string
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if fields := strings.Split(line, "\t"); len(fields) == 4 {
				shas = append(shas, fields[1])
			}
		}
		slices.Sort(shas)
		return shas
	}
	if got := listed("nudity"); !slices.Equal(got, []string{shas[0]}) {
		t.Errorf("nudity listed %v", got)
	}
	want := []string{shas[0], shas[1]}
	slices.Sort(want)
	if got := listed("faces"); !slices.Equal(got, want) {
		t.Errorf("faces listed %v, want %v", got, want)
	}
	if o.opened[shas[0]] != 1 || o.opened[shas[1]] != 1 {
		t.Errorf("tried again before a restart: %v", o.opened)
	}
	if !nudity.done[shas[1]] || !nudity.done[shas[2]] || !faces.done[shas[2]] {
		t.Error("a failure stopped other reads")
	}
}

// Preparing the next files overlaps the indexes' work: 10 files, each 100 ms to decode and 100 ms to read, take
// about a second, not two.
func TestPreparingOverlapsReading(t *testing.T) {
	kinds := make([]media.Kind, 10)
	for i := range kinds {
		kinds[i] = media.KindPhoto
	}
	lib, _ := library(t, kinds...)
	var calls []string
	var mu sync.Mutex
	x := newIndex("x", 0, Wants{Kinds: photos(), Pictures: []media.PreviewSpec{small}, Regions: true}, &calls, &mu)
	x.delay = 100 * time.Millisecond
	o := newOpener()
	o.delay = 100 * time.Millisecond
	r := New(lib, o, 2)
	r.Add(x)
	started := time.Now()
	r.ReadNew(context.Background(), "hen")
	if took := time.Since(started); took > 1600*time.Millisecond {
		t.Errorf("took %s: preparing and reading didn't overlap", took)
	}
	if o.most > 3 {
		t.Errorf("%d originals open at once, more than the 2 prepared ahead and the one being read", o.most)
	}
}

// Only one big original is held at a time.
func TestBigOriginalsAreHeldAlone(t *testing.T) {
	lib, _ := library(t)
	big := 60_000_000
	for range 4 {
		addFile(t, lib, media.KindPhoto, &big)
	}
	var calls []string
	var mu sync.Mutex
	x := newIndex("x", 0, Wants{Kinds: photos(), Pictures: []media.PreviewSpec{small}, Regions: true}, &calls, &mu)
	x.delay = 20 * time.Millisecond
	o := newOpener()
	r := New(lib, o, 2)
	r.Add(x)
	r.ReadNew(context.Background(), "hen")
	if o.most != 1 {
		t.Errorf("%d big originals open at once", o.most)
	}
	if len(x.reads) != 4 {
		t.Errorf("read %d", len(x.reads))
	}
}

// An upload is read as soon as the reader is woken.
func TestWakeReadsANewUpload(t *testing.T) {
	lib, _ := library(t)
	var calls []string
	var mu sync.Mutex
	x := newIndex("x", 0, Wants{Kinds: photos(), Pictures: []media.PreviewSpec{small}}, &calls, &mu)
	r := New(lib, newOpener(), 2)
	r.Add(x)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { stop(); <-done }()
	time.Sleep(50 * time.Millisecond)
	sha := addFile(t, lib, media.KindPhoto, nil)
	r.Wake()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		x.mu.Lock()
		read := x.done[sha]
		x.mu.Unlock()
		if read {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the upload wasn't read after Wake")
		}
	}
}

// sidecarWriter is an index that rewrites each file's sidecar, as the nudity scan does.
type sidecarWriter struct{ *index }

func (w sidecarWriter) Read(ctx context.Context, user string, in Input) error {
	if err := w.index.Read(ctx, user, in); err != nil {
		return err
	}
	return in.Store.SetNudity(in.Item.Sha256(), media.Detection{Score: 0.1, Model: "test"})
}

// After a run that rewrote sidecars, the start-up shortcut (.cache/index.json) is saved again, so a restart
// doesn't read those folders.
func TestARunSavesTheIndexCache(t *testing.T) {
	lib, _ := library(t, media.KindPhoto, media.KindPhoto)
	cache := filepath.Join(lib.Store("hen").Root(), ".cache", "index.json")
	before, err := os.Stat(cache)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	var calls []string
	var mu sync.Mutex
	r := New(lib, newOpener(), 2)
	r.Add(sidecarWriter{newIndex("nudity", 0, Wants{Kinds: photos(), Pictures: []media.PreviewSpec{small}}, &calls, &mu)})
	r.ReadNew(context.Background(), "hen")
	after, err := os.Stat(cache)
	if err != nil || !after.ModTime().After(before.ModTime()) {
		t.Errorf("index.json not saved after the run: %v → %v (%v)", before.ModTime(), after.ModTime(), err)
	}
}
