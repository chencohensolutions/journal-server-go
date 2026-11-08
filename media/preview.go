package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Previews makes previews with libvips and keeps them in each store's .cache/previews/. libvips runs inside the
// server when it can be loaded (LoadLibrary, vipslib.go), else, and for a file it failed on there, as the vips and
// vipsheader programs.
type Previews struct {
	vips, vipsheader string
	// libvips loaded into the server; nil when it runs only as programs.
	lib *vipsLib
	// sha256s of files the library failed on: they go to the programs from then on (until a restart).
	libFailed sync.Map
	// Take video posters (see poster.go); empty without ffmpeg.
	ffmpeg, ffprobe string
	// Running vips is heavy; this many at once.
	slots chan struct{}
	locks keyedMutex
	sizes sync.Map // sha256 (or sha256 + "/developed", a developed DNG) → [2]int upright size
}

var ErrPreviewsUnavailable = errors.New("libvips is not installed")

// ErrNoPicture is asked of a file that has no picture to show: a sound recording.
var ErrNoPicture = errors.New("a sound recording has no picture")

// FindVips returns the vips and vipsheader programs: the configured path, the bundle in vips/bin (made
// on Windows by scripts/bundle-vips, as the official Windows builds can't read HEIC), the PATH, or
// Homebrew's folders (a server started from an IDE often runs without Homebrew in its PATH).
func FindVips(configured string) (vips, vipsheader string, err error) {
	candidates := []string{configured, filepath.Join("vips", "bin", "vips"), "vips", "/opt/homebrew/bin/vips", "/usr/local/bin/vips"}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if found, err := exec.LookPath(candidate); err == nil {
			header := filepath.Join(filepath.Dir(found), "vipsheader")
			if _, err := exec.LookPath(header); err == nil {
				return found, header, nil
			}
		}
	}
	return "", "", ErrPreviewsUnavailable
}

// NewPreviews uses the given programs; empty ones make every preview fail with ErrPreviewsUnavailable.
func NewPreviews(vips, vipsheader string) *Previews {
	return &Previews{vips: vips, vipsheader: vipsheader, slots: make(chan struct{}, max(1, runtime.NumCPU()/2))}
}

// LoadLibrary loads libvips into the server (vipslib.go): the library beside the vips program found, or configured
// (config.json "libvips"), letting it use up to threads threads for each operation. It returns libvips's version.
// Without it, previews are made by the programs as before.
func (p *Previews) LoadLibrary(configured string, threads int) (version string, err error) {
	if p.vips == "" && configured == "" {
		return "", ErrPreviewsUnavailable
	}
	l, err := loadVipsLib(vipsLibraries(p.vips, configured), threads)
	if err != nil {
		return "", err
	}
	p.lib = l
	return l.Version, nil
}

// inProcess reports whether the pictures of the file sha are made by libvips in the server: it is loaded, and
// hasn't failed on that file.
func (p *Previews) inProcess(sha string) bool {
	if p.lib == nil {
		return false
	}
	_, failed := p.libFailed.Load(sha)
	return !failed
}

// libFailedOn notes that libvips in the server could not make sha's picture: the programs try from then on.
func (p *Previews) libFailedOn(sha, path string, err error) {
	p.libFailed.Store(sha, true)
	log.Printf("media: libvips in the server: %s: %v; the vips program makes its pictures from now on", path, err)
}

// Available reports whether previews can be made (libvips was found).
func (p *Previews) Available() bool { return p.vips != "" }

// PostersAvailable reports whether video posters can be taken (ffmpeg was found).
func (p *Previews) PostersAvailable() bool { return p.ffmpeg != "" }

func (s *Store) previewDir(sha string) string {
	return filepath.Join(s.root, ".cache", "previews", sha[:2], sha)
}

// Get returns the path of the preview, and release, to call once it has been sent. With the store's
// CachePreviews it is kept in .cache/previews (and found there next time); without, it is made in a
// temporary folder that release deletes.
func (p *Previews) Get(ctx context.Context, store *Store, item Item, spec PreviewSpec) (path string, release func(), err error) {
	release = func() {}
	if item.Sidecar.Facts.Kind == KindAudio {
		return "", release, ErrNoPicture
	}
	cached := filepath.Join(store.previewDir(item.Sha256()), spec.fileName())
	if info, err := os.Stat(cached); err == nil {
		touch(cached, info)
		return cached, release, nil
	}
	if p.vips == "" {
		return "", release, ErrPreviewsUnavailable
	}

	if store.CachePreviews {
		unlock := p.locks.Lock(cached)
		defer unlock()
		if _, err := os.Stat(cached); err == nil {
			return cached, release, nil
		}
	}
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	case <-ctx.Done():
		return "", release, ctx.Err()
	}

	if err := os.MkdirAll(filepath.Join(store.root, ".cache"), 0o755); err != nil {
		return "", release, err
	}
	work, err := os.MkdirTemp(filepath.Join(store.root, ".cache"), "preview-")
	if err != nil {
		return "", release, err
	}
	keepWork := false
	defer func() {
		if !keepWork {
			os.RemoveAll(work)
		}
	}()

	out, err := p.make(ctx, store, item, spec, work)
	if err != nil {
		return "", release, err
	}
	if !store.CachePreviews {
		keepWork = true
		return out, func() { os.RemoveAll(work) }, nil
	}
	if err := os.MkdirAll(filepath.Dir(cached), 0o755); err != nil {
		return "", release, err
	}
	if err := rename(out, cached); err != nil {
		return "", release, err
	}
	return cached, release, nil
}

// make makes the preview in work and returns its path.
func (p *Previews) make(ctx context.Context, store *Store, item Item, spec PreviewSpec, work string) (string, error) {
	original := filepath.Join(store.root, filepath.FromSlash(item.Path))
	source, sizeKey := original, item.Sha256()
	// A video's preview is made from its poster frame.
	if item.Sidecar.Facts.Kind == KindVideo {
		poster, err := p.poster(ctx, store, item, original, work, spec.Frame == "first")
		if err != nil {
			return "", err
		}
		return p.render(ctx, item.Sha256()+"/"+strings.TrimSuffix(filepath.Base(poster), ".jpg"), poster, work, spec)
	}
	// A DNG with demosaiced, JPEG XL compressed raw data (DNG 1.7) is developed here first, since LibRaw
	// can't unpack it; other DNGs go to libvips (LibRaw) like any photo. The JPEG a DNG carries is never
	// used: previews show the raw data.
	if isDNG(item.Path) {
		d, err := readLinearDNG(original)
		if err != nil {
			return "", fmt.Errorf("reading %s as DNG: %w", item.Path, err)
		}
		// In the server: developed in memory and made into the preview from there. With CachePreviews the
		// developed image is kept, as developed does, and the next preview is made from it instead.
		kept := filepath.Join(store.previewDir(item.Sha256()), developedName)
		_, keptErr := os.Stat(kept)
		if d != nil && p.inProcess(item.Sha256()) && !(store.CachePreviews && keptErr == nil) {
			out := filepath.Join(work, "out."+spec.Format)
			err := p.lib.onThread(func() error {
				img, err := p.developedImage(d, original)
				if err != nil {
					return err
				}
				defer img.Release()
				if store.CachePreviews {
					if err := p.keepDeveloped(img, kept); err != nil {
						return err
					}
				}
				return p.renderImage(img, out, spec)
			})
			if err == nil {
				return out, nil
			}
			p.libFailedOn(item.Sha256(), item.Path, err)
		}
		if d != nil {
			if source, err = p.developed(ctx, store, item, d, original, work); err != nil {
				return "", err
			}
			sizeKey = item.Sha256() + "/developed"
		}
	}
	return p.render(ctx, sizeKey, source, work, spec)
}

// render makes the preview of source (sizeKey, the sha256 and what source is of it, caches its upright size) in
// work, and returns its path.
func (p *Previews) render(ctx context.Context, sizeKey, source, work string, spec PreviewSpec) (string, error) {
	sha, _, _ := strings.Cut(sizeKey, "/")
	if p.inProcess(sha) {
		out := filepath.Join(work, "out."+spec.Format)
		err := p.lib.onThread(func() error { return p.renderFile(sizeKey, source, out, spec) })
		if err == nil {
			return out, nil
		}
		p.libFailedOn(sha, source, err)
	}
	width, height, err := p.uprightSize(ctx, sizeKey, source)
	if err != nil {
		return "", err
	}
	pl := planPreview(spec, width, height)

	saveOptions := spec.saveOptions()
	out := filepath.Join(work, "out."+spec.Format)
	size := []string{strconv.Itoa(pl.scaledW), "--height", strconv.Itoa(pl.scaledH), "--size", "force"}
	if !pl.needsCut() {
		if err := p.run(ctx, append([]string{"thumbnail", source, out + saveOptions}, size...)...); err != nil {
			return "", err
		}
	} else {
		scaled := filepath.Join(work, "scaled.v")
		if err := p.run(ctx, append([]string{"thumbnail", source, scaled}, size...)...); err != nil {
			return "", err
		}
		if err := p.run(ctx, "extract_area", scaled, out+saveOptions,
			strconv.Itoa(pl.left), strconv.Itoa(pl.top), strconv.Itoa(pl.outW), strconv.Itoa(pl.outH)); err != nil {
			return "", err
		}
	}
	return out, nil
}

// renderFile makes the preview of the file source in out with libvips in the server, as render does with the
// programs: scaled in one step (a JPEG is decoded already shrunk), then cut, then saved in spec's format.
func (p *Previews) renderFile(sizeKey, source, out string, spec PreviewSpec) error {
	width, height, ok := p.knownSize(sizeKey)
	if !ok {
		img, err := p.lib.load(source)
		if err != nil {
			return err
		}
		width, height = uprightOf(img)
		img.Release()
		p.sizes.Store(sizeKey, [2]int{width, height})
	}
	pl := planPreview(spec, width, height)
	scaled, err := p.lib.thumbnailFile(source, pl.scaledW, pl.scaledH, vipsSizeForce)
	if err != nil {
		return err
	}
	defer scaled.Release()
	return p.cutAndSave(scaled, pl, out, spec)
}

// keepDeveloped saves a developed DNG as path (a JPEG at Q=95 carrying its orientation, as developed keeps it),
// written beside and renamed so a reader never sees half of it.
func (p *Previews) keepDeveloped(img *vipsImage, path string) error {
	unlock := p.locks.Lock(path)
	defer unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	partial := path + ".partial.jpg"
	if err := p.lib.save(img, partial, "[Q=95]"); err != nil {
		os.Remove(partial)
		return err
	}
	return rename(partial, path)
}

// renderImage makes the preview of img (a developed DNG, in memory) in out, as renderFile does.
func (p *Previews) renderImage(img *vipsImage, out string, spec PreviewSpec) error {
	width, height := uprightOf(img)
	pl := planPreview(spec, width, height)
	scaled, err := p.lib.thumbnail(img, pl.scaledW, pl.scaledH, vipsSizeForce)
	if err != nil {
		return err
	}
	defer scaled.Release()
	return p.cutAndSave(scaled, pl, out, spec)
}

// cutAndSave cuts pl's area out of scaled, when the plan has one, and saves it in out.
func (p *Previews) cutAndSave(scaled *vipsImage, pl plan, out string, spec PreviewSpec) error {
	if pl.needsCut() {
		cut, err := p.lib.extractArea(scaled, pl.left, pl.top, pl.outW, pl.outH)
		if err != nil {
			return err
		}
		defer cut.Release()
		scaled = cut
	}
	return p.lib.save(scaled, out, spec.saveOptions())
}

// uprightOf is img's size as shown, after its EXIF orientation.
func uprightOf(img *vipsImage) (width, height int) {
	width, height = img.size()
	if img.orientation() >= 5 {
		width, height = height, width
	}
	return width, height
}

func (p *Previews) knownSize(key string) (width, height int, ok bool) {
	if size, ok := p.sizes.Load(key); ok {
		s := size.([2]int)
		return s[0], s[1], true
	}
	return 0, 0, false
}

// uprightSize is the original's size as shown, after its EXIF orientation (HEIC files are already
// upright when libvips reads them).
func (p *Previews) uprightSize(ctx context.Context, key, original string) (int, int, error) {
	if size, ok := p.sizes.Load(key); ok {
		s := size.([2]int)
		return s[0], s[1], nil
	}
	output, err := p.output(ctx, p.vipsheader, "-a", original)
	if err != nil {
		return 0, 0, err
	}
	fields := map[string]int{"orientation": 1}
	for line := range strings.SplitSeq(output, "\n") {
		name, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		if name == "width" || name == "height" || name == "orientation" {
			if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
				fields[name] = n
			}
		}
	}
	width, height := fields["width"], fields["height"]
	if width <= 0 || height <= 0 {
		return 0, 0, fmt.Errorf("vipsheader: no size for %s", original)
	}
	if o := fields["orientation"]; o >= 5 && o <= 8 {
		width, height = height, width
	}
	p.sizes.Store(key, [2]int{width, height})
	return width, height, nil
}

func (p *Previews) run(ctx context.Context, args ...string) error {
	_, err := p.output(ctx, p.vips, args...)
	return err
}

func (p *Previews) output(ctx context.Context, program string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, program, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %v: %s", filepath.Base(program), args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// touch marks a preview as used, at most once a day, so trimming drops the ones not looked at.
func touch(path string, info fs.FileInfo) {
	if now := time.Now(); now.Sub(info.ModTime()) > 24*time.Hour {
		os.Chtimes(path, now, now)
	}
}

// TrimPreviews deletes the least recently used previews until they take at most 90% of maxBytes.
func (s *Store) TrimPreviews(maxBytes int64) error {
	type cached struct {
		path string
		size int64
		used time.Time
	}
	var files []cached
	var total int64
	err := filepath.WalkDir(filepath.Join(s.root, ".cache", "previews"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipDir
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		files = append(files, cached{path, info.Size(), info.ModTime()})
		total += info.Size()
		return nil
	})
	if err != nil || total <= maxBytes {
		return err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].used.Before(files[j].used) })
	target := maxBytes / 10 * 9
	for _, f := range files {
		if total <= target {
			break
		}
		if err := os.Remove(f.path); err == nil {
			total -= f.size
		}
	}
	log.Printf("media: trimmed previews in %s to %d MB", s.root, total>>20)
	return nil
}
