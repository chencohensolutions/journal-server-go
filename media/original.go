package media

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/image/draw"
)

// Original is a file's picture for the indexes (package reading): decoded once, whole and upright, into memory, so
// that each model's input is made from the original itself, at exactly the size the model reads, and never from
// another picture made before (the owner's choice, Oct 23, 2026). A video's original is its poster frame; a JPEG XL
// DNG's, its developed raw data. Close it when done: a decoded original is 3 bytes a pixel (36 MB for 12 MP).
//
// With libvips in the server. Without it, or for a file it failed on, each picture is made by the vips program
// from the original file instead (decoded once per size, but still from the original).
type Original struct {
	p     *Previews
	ctx   context.Context
	store *Store
	item  Item
	img   *vipsImage // upright, in memory; nil when the programs make the pictures
	once  sync.Once
}

// SqueezeFit is a picture fit for models that read exactly width×height whatever the photo's shape (SigLIP): the
// photo is stretched to it. Only for Original.Picture; never a device preview.
const SqueezeFit = "squeeze"

// shrinkAbove is how many pixels an original may have before it is decoded already shrunk (to twice the largest
// side asked for), so that a 200 MP photo can't take the server's memory. A variable for the tests.
var shrinkAbove = 100_000_000

// Open decodes item's original for the indexes. largest is the longest side any of them will ask for: an original
// of more than 100 MP is decoded at twice that, not whole.
func (p *Previews) Open(ctx context.Context, store *Store, item Item, largest int) (*Original, error) {
	if item.Sidecar.Facts.Kind == KindAudio {
		return nil, ErrNoPicture
	}
	o := &Original{p: p, ctx: ctx, store: store, item: item}
	sha := item.Sha256()
	if !p.inProcess(sha) {
		if p.vips == "" {
			return nil, ErrPreviewsUnavailable
		}
		return o, nil
	}
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	err := p.lib.onThread(func() error {
		var err error
		o.img, err = p.decode(ctx, store, item, largest)
		return err
	})
	switch {
	case err == nil:
		return o, nil
	case errors.Is(err, ErrPostersUnavailable), ctx.Err() != nil:
		return nil, err
	default:
		p.libFailedOn(sha, item.Path, err)
		return o, nil
	}
}

// decode is item's original, upright and in memory. Runs on a libvips thread.
func (p *Previews) decode(ctx context.Context, store *Store, item Item, largest int) (*vipsImage, error) {
	original := filepath.Join(store.root, filepath.FromSlash(item.Path))
	if item.Sidecar.Facts.Kind == KindVideo {
		if err := os.MkdirAll(filepath.Join(store.root, ".cache"), 0o755); err != nil {
			return nil, err
		}
		work, err := os.MkdirTemp(filepath.Join(store.root, ".cache"), "original-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(work)
		if original, err = p.poster(ctx, store, item, original, work); err != nil {
			return nil, err
		}
	} else if isDNG(item.Path) {
		d, err := readLinearDNG(original)
		if err != nil {
			return nil, fmt.Errorf("reading %s as DNG: %w", item.Path, err)
		}
		if d != nil {
			developed, err := p.developedImage(d, original)
			if err != nil {
				return nil, err
			}
			defer developed.Release()
			return p.uprightInMemory(developed)
		}
	}
	loaded, err := p.lib.load(original)
	if err != nil {
		return nil, err
	}
	defer loaded.Release()
	if w, h := loaded.size(); w*h > shrinkAbove && largest > 0 {
		shrunk, err := p.lib.thumbnailFile(original, 2*largest, 2*largest, vipsSizeDown)
		if err != nil {
			return nil, err
		}
		defer shrunk.Release()
		return p.lib.inMemory(shrunk)
	}
	return p.uprightInMemory(loaded)
}

// uprightInMemory is img turned upright and decoded whole into memory.
func (p *Previews) uprightInMemory(img *vipsImage) (*vipsImage, error) {
	upright, err := p.lib.autorot(img)
	if err != nil {
		return nil, err
	}
	defer upright.Release()
	return p.lib.inMemory(upright)
}

// Size is the decoded original's size, upright; 0, 0 when the programs make the pictures.
func (o *Original) Size() (width, height int) {
	if o.img == nil {
		return 0, 0
	}
	return o.img.size()
}

// Picture is the original resized once to spec: Fit "contain" fits inside Width×Height and never enlarges;
// SqueezeFit stretches to exactly Width×Height; "cover" fills exactly Width×Height, cutting off the sides equally
// (a model's centre crop). No crop of a part.
func (o *Original) Picture(spec PreviewSpec) (*image.RGBA, error) {
	if spec.Crop != nil || (spec.Fit != "contain" && spec.Fit != SqueezeFit && spec.Fit != "cover") || spec.Width < 1 || spec.Height < 1 {
		return nil, fmt.Errorf("an original's picture is contain, cover or squeeze, without a crop: %+v", spec)
	}
	if o.img == nil {
		return o.programPicture(spec)
	}
	size := vipsSizeDown
	if spec.Fit == SqueezeFit {
		size = vipsSizeForce
	}
	var rgba *image.RGBA
	err := o.p.lib.onThread(func() error {
		var resized *vipsImage
		var err error
		if spec.Fit == "cover" {
			resized, err = o.p.lib.thumbnailCover(o.img, spec.Width, spec.Height)
		} else {
			resized, err = o.p.lib.thumbnail(o.img, spec.Width, spec.Height, size)
		}
		if err != nil {
			return err
		}
		defer resized.Release()
		rgba, err = o.p.lib.rgba(resized)
		return err
	})
	return rgba, err
}

// Region is the part box of the original (fractions of the upright photo, kept inside it) resized once to exactly
// width×height: for an index that reads details, such as a face.
func (o *Original) Region(box Rect, width, height int) (*image.RGBA, error) {
	if width < 1 || height < 1 {
		return nil, fmt.Errorf("a region of %d×%d", width, height)
	}
	if o.img == nil {
		return o.programRegion(box, width, height)
	}
	var rgba *image.RGBA
	err := o.p.lib.onThread(func() error {
		w, h := o.img.size()
		left, top, rw, rh := pixelBox(box, w, h)
		part, err := o.p.lib.extractArea(o.img, left, top, rw, rh)
		if err != nil {
			return err
		}
		defer part.Release()
		resized, err := o.p.lib.thumbnail(part, width, height, vipsSizeForce)
		if err != nil {
			return err
		}
		defer resized.Release()
		rgba, err = o.p.lib.rgba(resized)
		return err
	})
	return rgba, err
}

// pixelBox is box (fractions) in a w×h picture: whole pixels, at least one, inside it.
func pixelBox(box Rect, w, h int) (left, top, width, height int) {
	left = min(max(0, int(math.Floor(box.X*float64(w)))), w-1)
	top = min(max(0, int(math.Floor(box.Y*float64(h)))), h-1)
	right := min(w, max(left+1, int(math.Ceil((box.X+box.W)*float64(w)))))
	bottom := min(h, max(top+1, int(math.Ceil((box.Y+box.H)*float64(h)))))
	return left, top, right - left, bottom - top
}

// Close lets the decoded original go.
func (o *Original) Close() {
	o.once.Do(func() {
		if o.img != nil {
			o.img.Release()
		}
	})
}

// programPicture is spec made by the vips program from the original file: contain or cover, and for SqueezeFit
// then stretched in Go.
func (o *Original) programPicture(spec PreviewSpec) (*image.RGBA, error) {
	fit := "contain"
	if spec.Fit == "cover" {
		fit = "cover"
	}
	contain := PreviewSpec{Width: spec.Width, Height: spec.Height, Fit: fit, Format: "jpeg", Quality: "max"}
	img, err := o.programGet(contain)
	if err != nil {
		return nil, err
	}
	if spec.Fit == SqueezeFit {
		return resized(img, img.Bounds(), spec.Width, spec.Height), nil
	}
	return resized(img, img.Bounds(), img.Bounds().Dx(), img.Bounds().Dy()), nil
}

// programRegion is the region cut from a picture the vips program makes of the whole original, at the size where
// the region comes out about width×height (at most MaxPreviewSide).
func (o *Original) programRegion(box Rect, width, height int) (*image.RGBA, error) {
	side := min(MaxPreviewSide, int(math.Ceil(max(float64(width)/max(box.W, 1e-6), float64(height)/max(box.H, 1e-6)))))
	whole, err := o.programGet(PreviewSpec{Width: side, Height: side, Fit: "contain", Format: "jpeg", Quality: "max"})
	if err != nil {
		return nil, err
	}
	b := whole.Bounds()
	left, top, rw, rh := pixelBox(box, b.Dx(), b.Dy())
	return resized(whole, image.Rect(b.Min.X+left, b.Min.Y+top, b.Min.X+left+rw, b.Min.Y+top+rh), width, height), nil
}

func (o *Original) programGet(spec PreviewSpec) (image.Image, error) {
	path, release, err := o.p.Get(o.ctx, o.store, o.item, spec)
	if err != nil {
		return nil, err
	}
	defer release()
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	return img, err
}

// resized is the part r of img resized to width×height (Catmull-Rom).
func resized(img image.Image, r image.Rectangle, width, height int) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(out, out.Bounds(), img, r, draw.Src, nil)
	return out
}
