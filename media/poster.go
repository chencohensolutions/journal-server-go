package media

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// A video's preview is made from its poster: one frame, taken by ffmpeg (a separate program, like
// libvips) and then scaled, cut and encoded like any photo. The frame is upright (ffmpeg applies the
// video's rotation), and an HDR video (HLG or PQ) is tone-mapped to standard range so it doesn't look
// washed out. It is kept as poster.jpg beside the video's previews when the user's preview_cache is on.

const posterName = "poster.jpg"

// The frame is taken this far in (or halfway through a shorter video), past the black or blurry first
// frame many recordings start with.
const posterAtMillis = 1000

var ErrPostersUnavailable = errors.New("ffmpeg is not installed")

// FindFFmpeg returns the ffmpeg and ffprobe programs: the configured path, the PATH, or Homebrew's
// folders (a server started from an IDE often runs without Homebrew in its PATH).
func FindFFmpeg(configured string) (ffmpeg, ffprobe string, err error) {
	for _, candidate := range []string{configured, "ffmpeg", "/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg"} {
		if candidate == "" {
			continue
		}
		if found, err := exec.LookPath(candidate); err == nil {
			probe := filepath.Join(filepath.Dir(found), "ffprobe")
			if _, err := exec.LookPath(probe); err == nil {
				return found, probe, nil
			}
		}
	}
	return "", "", ErrPostersUnavailable
}

// WithFFmpeg makes video posters with these programs; without, video previews fail with
// ErrPostersUnavailable.
func (p *Previews) WithFFmpeg(ffmpeg, ffprobe string) *Previews {
	p.ffmpeg, p.ffprobe = ffmpeg, ffprobe
	return p
}

// poster returns the poster frame of a video: kept beside its previews with the store's CachePreviews
// (and found there next time), else made in into, the request's temporary folder.
func (p *Previews) poster(ctx context.Context, store *Store, item Item, original, into string) (string, error) {
	if p.ffmpeg == "" {
		return "", ErrPostersUnavailable
	}
	path := filepath.Join(into, posterName)
	if store.CachePreviews {
		path = filepath.Join(store.previewDir(item.Sha256()), posterName)
		unlock := p.locks.Lock(path)
		defer unlock()
	}
	if info, err := os.Stat(path); err == nil {
		touch(path, info)
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}

	at := int64(posterAtMillis)
	if d := item.Sidecar.Facts.DurationMs; d != nil && *d > 0 && *d < 2*posterAtMillis {
		at = *d / 2
	}
	frame := filepath.Join(filepath.Dir(path), "poster.tmp.jpg")
	defer os.Remove(frame)
	args := func(filter string) []string {
		a := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
			"-ss", fmt.Sprintf("%d.%03d", at/1000, at%1000), "-i", original, "-frames:v", "1", "-an"}
		if filter != "" {
			a = append(a, "-vf", filter)
		}
		return append(a, "-q:v", "2", frame)
	}
	err := errors.New("no frame")
	if p.isHDR(ctx, original) {
		_, err = p.output(ctx, p.ffmpeg, args(hdrToSDR)...)
		if err != nil {
			// An ffmpeg without zscale: the frame as it is.
			log.Printf("media: tone-mapping the poster of %s: %v", item.Path, err)
		}
	}
	if err != nil {
		if _, err = p.output(ctx, p.ffmpeg, args("")...); err != nil {
			return "", err
		}
	}
	// A video shorter than where the frame is taken has none there: take its first.
	if _, statErr := os.Stat(frame); statErr != nil {
		at = 0
		if _, err := p.output(ctx, p.ffmpeg, args("")...); err != nil {
			return "", err
		}
	}
	if err := rename(frame, path); err != nil {
		return "", err
	}
	return path, nil
}

// hdrToSDR tone-maps HLG or PQ video to BT.709 (ffmpeg's zscale and tonemap filters).
const hdrToSDR = "zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0," +
	"zscale=t=bt709:m=bt709:r=tv,format=yuv420p"

// isHDR reports whether the video's first video stream uses an HDR transfer (PQ or HLG).
func (p *Previews) isHDR(ctx context.Context, original string) bool {
	out, err := p.output(ctx, p.ffprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=color_transfer", "-of", "default=noprint_wrappers=1:nokey=1", original)
	if err != nil {
		return false
	}
	transfer := strings.TrimSpace(out)
	return transfer == "smpte2084" || transfer == "arib-std-b67"
}
