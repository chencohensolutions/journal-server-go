package media

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// MaxPreviewSide is the largest width or height a preview may be asked for.
const MaxPreviewSide = 4096

// ModelPreview is the picture models read instead of an original (scripts/scan-nudity, search): small, and the
// same for each of them, so one kept preview serves them all.
var ModelPreview = PreviewSpec{Width: 640, Height: 640, Fit: "contain", Format: "jpeg", Quality: "high"}

// FacePreview is the picture face search reads (package faces): bigger than ModelPreview, so that faces in the
// background of a group photo are still big enough to tell apart.
var FacePreview = PreviewSpec{Width: 1280, Height: 1280, Fit: "contain", Format: "jpeg", Quality: "high"}

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
