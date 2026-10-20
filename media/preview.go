package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net/url"
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

// MaxPreviewSide is the largest width or height a preview may be asked for.
const MaxPreviewSide = 4096

// PreviewSpec is the exact picture a client will draw: Width×Height physical pixels.
type PreviewSpec struct {
	Width, Height int
	// "cover" fills the box (cutting off what doesn't fit, centered); "contain" fits inside it.
	Fit string
	// Part of the upright original to show, as fractions of its width and height; nil for all of it.
	Crop *Rect
	// "webp", "jpeg", "heif" (HEVC), "avif" (AV1) or "jxl" (JPEG XL).
	Format string
	// "low", "medium", "high" or "max": how much detail is kept against how small the file is.
	Quality string
}

// previewFormats are the encodings a preview can be asked for, with their MIME type and libvips options.
var previewFormats = map[string]struct{ mime, options string }{
	"webp": {"image/webp", ""},
	"jpeg": {"image/jpeg", ""},
	"heif": {"image/heif", "compression=hevc,subsample_mode=on,"}, // 4:2:0: phones decode Main profile HEVC
	"avif": {"image/avif", "compression=av1,subsample_mode=on,"},
	"jxl":  {"image/jxl", ""},
}

// previewQualities are the libvips Q of each quality step.
var previewQualities = map[string]int{"low": 50, "medium": 70, "high": 85, "max": 95}

type Rect struct{ X, Y, W, H float64 }

var ErrInvalidPreview = errors.New("invalid preview request")

// ParsePreviewSpec reads ?w=1080&h=1440&fit=cover&crop=x,y,w,h&fmt=webp&q=high.
func ParsePreviewSpec(q url.Values) (PreviewSpec, error) {
	spec := PreviewSpec{Fit: "cover", Format: "webp", Quality: "high"}
	var err error
	if spec.Width, err = strconv.Atoi(q.Get("w")); err != nil || spec.Width < 1 || spec.Width > MaxPreviewSide {
		return spec, fmt.Errorf("%w: w must be 1–%d", ErrInvalidPreview, MaxPreviewSide)
	}
	if spec.Height, err = strconv.Atoi(q.Get("h")); err != nil || spec.Height < 1 || spec.Height > MaxPreviewSide {
		return spec, fmt.Errorf("%w: h must be 1–%d", ErrInvalidPreview, MaxPreviewSide)
	}
	if fit := q.Get("fit"); fit != "" {
		if fit != "cover" && fit != "contain" {
			return spec, fmt.Errorf("%w: fit is cover or contain", ErrInvalidPreview)
		}
		spec.Fit = fit
	}
	if format := q.Get("fmt"); format != "" {
		if _, ok := previewFormats[format]; !ok {
			return spec, fmt.Errorf("%w: fmt is webp, jpeg, heif, avif or jxl", ErrInvalidPreview)
		}
		spec.Format = format
	}
	if quality := q.Get("q"); quality != "" {
		if _, ok := previewQualities[quality]; !ok {
			return spec, fmt.Errorf("%w: q is low, medium, high or max", ErrInvalidPreview)
		}
		spec.Quality = quality
	}
	if crop := q.Get("crop"); crop != "" {
		parts := strings.Split(crop, ",")
		values := make([]float64, len(parts))
		for i, part := range parts {
			// Rounded so that nearly equal crops share one cached file (1/10000 of the photo).
			v, err := strconv.ParseFloat(part, 64)
			values[i] = math.Round(v*10000) / 10000
			if err != nil || math.IsNaN(v) {
				values = nil
				break
			}
		}
		if len(values) != 4 {
			return spec, fmt.Errorf("%w: crop is x,y,w,h", ErrInvalidPreview)
		}
		r := Rect{values[0], values[1], values[2], values[3]}
		if r.X < 0 || r.Y < 0 || r.W <= 0 || r.H <= 0 || r.X+r.W > 1 || r.Y+r.H > 1 {
			return spec, fmt.Errorf("%w: crop must lie within 0–1", ErrInvalidPreview)
		}
		if r != (Rect{0, 0, 1, 1}) {
			spec.Crop = &r
		}
	}
	return spec, nil
}

func (s PreviewSpec) fileName() string {
	name := fmt.Sprintf("%dx%d-%s", s.Width, s.Height, s.Fit)
	if s.Crop != nil {
		name += fmt.Sprintf("-%g,%g,%g,%g", s.Crop.X, s.Crop.Y, s.Crop.W, s.Crop.H)
	}
	return name + "-" + s.Quality + "." + s.Format
}

func (s PreviewSpec) ContentType() string { return previewFormats[s.Format].mime }

// saveOptions are libvips' options for writing the preview. Only the colour profile is kept: EXIF
// would carry an orientation that is already applied.
func (s PreviewSpec) saveOptions() string {
	return fmt.Sprintf("[%sQ=%d,keep=icc]", previewFormats[s.Format].options, previewQualities[s.Quality])
}

// plan is how to get from an upright original of size W×H to the preview: scale the whole picture to
// scaledW×scaledH in one step, then cut out the width×height area at left, top.
type plan struct {
	scaledW, scaledH      int
	left, top, outW, outH int
}

func (p plan) needsCut() bool {
	return p.left != 0 || p.top != 0 || p.outW != p.scaledW || p.outH != p.scaledH
}

// planPreview never scales up: when the box asks for more pixels than the original has, the preview
// keeps the box's shape at the original's own resolution, and the client scales it on screen.
func planPreview(spec PreviewSpec, width, height int) plan {
	crop := Rect{0, 0, 1, 1}
	if spec.Crop != nil {
		crop = *spec.Crop
	}
	regionW, regionH := crop.W*float64(width), crop.H*float64(height)
	boxW, boxH := float64(spec.Width), float64(spec.Height)

	var scale, outW, outH float64
	if spec.Fit == "contain" {
		scale = math.Min(1, math.Min(boxW/regionW, boxH/regionH))
		outW, outH = regionW*scale, regionH*scale
	} else {
		scale = math.Max(boxW/regionW, boxH/regionH)
		if scale > 1 {
			boxW, boxH, scale = boxW/scale, boxH/scale, 1
		}
		outW, outH = boxW, boxH
	}

	p := plan{
		scaledW: max(1, int(math.Round(float64(width)*scale))),
		scaledH: max(1, int(math.Round(float64(height)*scale))),
	}
	p.outW = min(p.scaledW, max(1, int(math.Round(outW))))
	p.outH = min(p.scaledH, max(1, int(math.Round(outH))))
	// Centered within the crop, then kept inside the scaled picture.
	left := crop.X*float64(p.scaledW) + (crop.W*float64(p.scaledW)-float64(p.outW))/2
	top := crop.Y*float64(p.scaledH) + (crop.H*float64(p.scaledH)-float64(p.outH))/2
	p.left = min(max(0, int(math.Round(left))), p.scaledW-p.outW)
	p.top = min(max(0, int(math.Round(top))), p.scaledH-p.outH)
	return p
}

// Previews makes previews with libvips and keeps them in each store's .cache/previews/.
type Previews struct {
	vips, vipsheader string
	// Running vips is heavy; this many at once.
	slots chan struct{}
	locks keyedMutex
	sizes sync.Map // sha256 (or sha256 + "/developed", a developed DNG) → [2]int upright size
}

var ErrPreviewsUnavailable = errors.New("libvips is not installed")

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

func (s *Store) previewDir(sha string) string {
	return filepath.Join(s.root, ".cache", "previews", sha[:2], sha)
}

// Get returns the path of the preview, and release, to call once it has been sent. With the store's
// CachePreviews it is kept in .cache/previews (and found there next time); without, it is made in a
// temporary folder that release deletes.
func (p *Previews) Get(ctx context.Context, store *Store, item Item, spec PreviewSpec) (path string, release func(), err error) {
	release = func() {}
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
	// A DNG with demosaiced, JPEG XL compressed raw data (DNG 1.7) is developed here first, since LibRaw
	// can't unpack it; other DNGs go to libvips (LibRaw) like any photo. The JPEG a DNG carries is never
	// used: previews show the raw data.
	if isDNG(item.Path) {
		d, err := readLinearDNG(original)
		if err != nil {
			return "", fmt.Errorf("reading %s as DNG: %w", item.Path, err)
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

// render makes the preview of source (sizeKey caches its upright size) in work, and returns its path.
func (p *Previews) render(ctx context.Context, sizeKey, source, work string, spec PreviewSpec) (string, error) {
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
	for _, line := range strings.Split(output, "\n") {
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

// TrimPreviews applies the limit to every user's store.
func (l *Library) TrimPreviews(maxBytes int64) {
	for user, store := range l.stores {
		if err := store.TrimPreviews(maxBytes); err != nil {
			log.Printf("media: trimming previews of %s: %v", user, err)
		}
	}
}
