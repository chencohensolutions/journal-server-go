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
// washed out. It is kept as poster.jpg (or first-frame.jpg) beside the video's previews when the user's
// preview_cache is on.
//
// A phone may ask for the very first frame instead (?frame=first): ffmpeg then decodes from the start, so the
// frame is the first one the video shows, as a player would, even where that isn't a keyframe.

const (
	posterName     = "poster.jpg"
	firstFrameName = "first-frame.jpg"
)

// The frame is the first keyframe this far in or later (or halfway through a shorter video), past the black or
// blurry first frame many recordings start with. Only keyframes are decoded: landing exactly on the time would mean
// decoding every frame from the keyframe before it (about 3.7 s for a 4K HDR video from the owner's phone, against
// 1.3 s for the keyframe, which comes every 1.1 to 1.6 s there; Oct 23, 2026).
const posterAtMillis = 1000

var ErrPostersUnavailable = errors.New("ffmpeg is not installed")

// FindFFmpeg returns the ffmpeg and ffprobe programs: the configured path, ffmpeg/bin (where journal-server setup
// puts them on Windows), the PATH, or Homebrew's folders (a server started from an IDE often runs without Homebrew
// in its PATH).
func FindFFmpeg(configured string) (ffmpeg, ffprobe string, err error) {
	for _, candidate := range []string{configured, filepath.Join("ffmpeg", "bin", "ffmpeg"), "ffmpeg", "/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg"} {
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

// poster returns the poster frame of a video (with first, its very first frame): kept beside its previews with
// the store's CachePreviews (and found there next time), else made in into, the request's temporary folder.
func (p *Previews) poster(ctx context.Context, store *Store, item Item, original, into string, first bool) (string, error) {
	if p.ffmpeg == "" {
		return "", ErrPostersUnavailable
	}
	name := posterName
	if first {
		name = firstFrameName
	}
	path := filepath.Join(into, name)
	if store.CachePreviews {
		path = filepath.Join(store.previewDir(item.Sha256()), name)
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
	frame := filepath.Join(filepath.Dir(path), strings.TrimSuffix(name, ".jpg")+".tmp.jpg")
	defer os.Remove(frame)
	args := func(filter string) []string {
		a := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}
		if first {
			// Every frame is decoded from the start until the first one shown is out (with its edit list and
			// frame order, as a player shows it): only the first group of frames is read.
			a = append(a, "-i", original)
		} else {
			a = append(a, "-skip_frame", "nokey", "-ss", fmt.Sprintf("%d.%03d", at/1000, at%1000), "-i", original)
		}
		a = append(a, "-map", "0:v:0", "-frames:v", "1", "-an")
		if filter != "" {
			a = append(a, "-vf", filter)
		}
		return append(a, "-q:v", "2", frame)
	}
	// take runs ffmpeg and reports whether it wrote the frame. When no keyframe comes after the time, nothing is
	// decoded, and ffmpeg either writes nothing or fails setting up its encoder: either way, no frame.
	err := errors.New("no frame")
	take := func(filter string) bool {
		os.Remove(frame)
		if _, err = p.output(ctx, p.ffmpeg, args(filter)...); err != nil {
			return false
		}
		if _, statErr := os.Stat(frame); statErr != nil {
			err = fmt.Errorf("ffmpeg: no frame at %d ms", at)
			return false
		}
		return true
	}
	filters := []string{""}
	if p.isHDR(ctx, original) {
		// Tone-mapped, else (an ffmpeg without zscale) the frame as it is.
		filters = []string{hdrToSDR, ""}
	}
	got := false
	// The keyframe from the time on, else the video's first frame (always a keyframe).
	times := []int64{at, 0}
	if first {
		times = []int64{0}
	}
	for _, from := range times {
		at = from
		var toneMapping error
		for _, filter := range filters {
			if got = take(filter); got {
				if filter == "" && toneMapping != nil {
					log.Printf("media: tone-mapping the poster of %s: %v", item.Path, toneMapping)
				}
				break
			}
			if filter != "" {
				toneMapping = err
			}
		}
		if got || from == 0 {
			break
		}
	}
	if !got {
		return "", err
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

// ErrNoVideoFrame is a file with no video stream to cut a first frame from.
var ErrNoVideoFrame = errors.New("no video frame")

// FirstFrameVideo cuts the video's first frame out as a video of its own, in into: an MP4 holding only the first
// frame's compressed data, copied as it is (nothing is decoded or encoded here), with the video's rotation and colour
// tags, for a phone to decode with its own decoder (GET /media/{sha256}/frame). It is the least a phone needs to
// show the first frame itself: about the size of one keyframe (0.3 to 2 MB for a phone's 4K video), against the
// tens of kilobytes of a preview, which the server decodes instead. The first frame of a phone's recording is a
// keyframe; a video whose first shown frame isn't (an open group of frames) gets its first keyframe.
func (p *Previews) FirstFrameVideo(ctx context.Context, item Item, original, into string) (string, error) {
	if p.ffmpeg == "" {
		return "", ErrPostersUnavailable
	}
	if item.Sidecar.Facts.Kind != KindVideo {
		return "", ErrNoVideoFrame
	}
	out := filepath.Join(into, "first-frame.mp4")
	if _, err := p.output(ctx, p.ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-i", original,
		"-map", "0:v:0", "-c", "copy", "-frames:v", "1", "-an", "-sn", "-dn", "-movflags", "+faststart",
		"-f", "mp4", out); err != nil {
		return "", err
	}
	if info, err := os.Stat(out); err != nil || info.Size() == 0 {
		return "", fmt.Errorf("ffmpeg: no first frame in %s", item.Path)
	}
	return out, nil
}
