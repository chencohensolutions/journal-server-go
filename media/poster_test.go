package media

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// rotatedVideo is a 2 s H.264 video, 400×200 stored (left half red, right half blue), tagged to be
// shown rotated by 90°: upright it is 200×400, the halves on top of each other.
func rotatedVideo(t *testing.T, ffmpeg string) []byte {
	dir := t.TempDir()
	plain, rotated := filepath.Join(dir, "plain.mp4"), filepath.Join(dir, "rotated.mp4")
	run := func(args ...string) {
		if out, err := exec.Command(ffmpeg, append([]string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Skipf("ffmpeg can't make the test video: %v %s", err, out)
		}
	}
	run("-f", "lavfi", "-i", "color=c=red:s=200x200:d=2:r=10", "-f", "lavfi", "-i", "color=c=blue:s=200x200:d=2:r=10",
		"-filter_complex", "hstack", "-c:v", "libx264", "-pix_fmt", "yuv420p", plain)
	run("-display_rotation", "90", "-i", plain, "-c", "copy", rotated)
	data, err := os.ReadFile(rotated)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestVideoPreviewsComeFromAnUprightPosterFrame(t *testing.T) {
	bothWays(t, func(t *testing.T, inProcess bool) {
		vips, header, err := FindVips("")
		if err != nil {
			t.Skip("libvips not installed")
		}
		ffmpeg, ffprobe, err := FindFFmpeg("")
		if err != nil {
			t.Skip("ffmpeg not installed")
		}
		video := rotatedVideo(t, ffmpeg)
		f := newFixture(t)
		req := photoRequest(video, "VID.mp4")
		req.Kind, req.MimeType = KindVideo, "video/mp4"
		duration := int64(2000)
		req.DurationMs = &duration
		f.start(req)
		result, err := f.chunk(video, 0, int64(len(video)))
		if err != nil || result.Item == nil {
			t.Fatalf("upload: %v", err)
		}
		item := *result.Item
		spec, err := ParsePreviewSpec(url.Values{"w": {"100"}, "h": {"200"}, "fit": {"contain"}, "fmt": {"jpeg"}})
		if err != nil {
			t.Fatal(err)
		}

		// Without ffmpeg there is no poster.
		if _, _, err := withLib(t, NewPreviews(vips, header), inProcess).Get(context.Background(), f.store, item, spec); err != ErrPostersUnavailable {
			t.Fatalf("without ffmpeg: %v", err)
		}

		previews := withLib(t, NewPreviews(vips, header), inProcess).WithFFmpeg(ffmpeg, ffprobe)
		for _, cache := range []bool{false, true} {
			f.store.CachePreviews = cache
			path, release, err := previews.Get(context.Background(), f.store, item, spec)
			if err != nil {
				t.Fatalf("cache %v: %v", cache, err)
			}
			img := decodeJPEG(t, path)
			release()
			if b := img.Bounds(); b.Dx() != 100 || b.Dy() != 200 {
				t.Fatalf("cache %v: size %v (not upright?)", cache, b)
			}
			top, bottom := img.At(50, 20), img.At(50, 180)
			if !(isRed(top) && isBlue(bottom)) && !(isBlue(top) && isRed(bottom)) {
				t.Fatalf("cache %v: halves not on top of each other: %v %v", cache, top, bottom)
			}
			_, err = os.Stat(filepath.Join(f.store.previewDir(item.Sha256()), posterName))
			if cache != (err == nil) {
				t.Errorf("cache %v: poster.jpg kept = %v", cache, err == nil)
			}
		}
	})
}

// The poster is the first keyframe at 1 s or later, not the frame at exactly 1 s: in a video black until 1.2 s,
// white after, with keyframes at 0 and 1.2 s, the frame at 1 s is black and the poster white.
func TestThePosterIsTheFirstKeyframeAfterASecond(t *testing.T) {
	vips, header, err := FindVips("")
	if err != nil {
		t.Skip("libvips not installed")
	}
	ffmpeg, ffprobe, err := FindFFmpeg("")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	video := filepath.Join(t.TempDir(), "keys.mp4")
	if out, err := exec.Command(ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c=black:s=64x64:d=1.2:r=10", "-f", "lavfi", "-i", "color=c=white:s=64x64:d=1.8:r=10",
		"-filter_complex", "concat=n=2:v=1", "-c:v", "libx264", "-g", "12", "-keyint_min", "12", "-sc_threshold", "0",
		"-pix_fmt", "yuv420p", video).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg can't make the test video: %v %s", err, out)
	}
	data, err := os.ReadFile(video)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	req := photoRequest(data, "KEYS.mp4")
	req.Kind, req.MimeType = KindVideo, "video/mp4"
	duration := int64(3000)
	req.DurationMs = &duration
	f.start(req)
	result, err := f.chunk(data, 0, int64(len(data)))
	if err != nil || result.Item == nil {
		t.Fatalf("upload: %v", err)
	}
	spec, _ := ParsePreviewSpec(url.Values{"w": {"64"}, "h": {"64"}, "fit": {"contain"}, "fmt": {"jpeg"}})
	previews := NewPreviews(vips, header).WithFFmpeg(ffmpeg, ffprobe)
	path, release, err := previews.Get(context.Background(), f.store, *result.Item, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if r, _, _, _ := decodeJPEG(t, path).At(32, 32).RGBA(); r < 200*257 {
		t.Errorf("the poster is dark (%d): the frame at 1 s, not the keyframe at 1.2 s", r>>8)
	}

	// Asked for the very first frame, it is the black one, and kept apart from the usual poster.
	first, _ := ParsePreviewSpec(url.Values{"w": {"64"}, "h": {"64"}, "fit": {"contain"}, "fmt": {"jpeg"}, "frame": {"first"}})
	f.store.CachePreviews = true
	path, releaseFirst, err := previews.Get(context.Background(), f.store, *result.Item, first)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()
	if r, _, _, _ := decodeJPEG(t, path).At(32, 32).RGBA(); r > 50*257 {
		t.Errorf("the first frame is light (%d), not the video's black start", r>>8)
	}
	if _, err := os.Stat(filepath.Join(f.store.previewDir(result.Item.Sha256()), firstFrameName)); err != nil {
		t.Errorf("first-frame.jpg not kept: %v", err)
	}
}

// The first frame as a video of its own: one frame, copied (the same codec), with the video's rotation.
func TestTheFirstFrameIsCutAsAVideoOfOneFrame(t *testing.T) {
	ffmpeg, ffprobe, err := FindFFmpeg("")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	video := rotatedVideo(t, ffmpeg)
	f := newFixture(t)
	req := photoRequest(video, "VID.mp4")
	req.Kind, req.MimeType = KindVideo, "video/mp4"
	f.start(req)
	result, err := f.chunk(video, 0, int64(len(video)))
	if err != nil || result.Item == nil {
		t.Fatalf("upload: %v", err)
	}
	item := *result.Item
	previews := NewPreviews("", "").WithFFmpeg(ffmpeg, ffprobe)
	original := filepath.Join(f.store.root, filepath.FromSlash(item.Path))
	out, err := previews.FirstFrameVideo(context.Background(), item, original, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	probe, err := exec.Command(ffprobe, "-v", "error", "-count_packets", "-select_streams", "v:0", "-show_entries",
		"stream=codec_name,nb_read_packets:stream_side_data=rotation", "-of", "default=noprint_wrappers=1", out).Output()
	if err != nil {
		t.Fatal(err)
	}
	got := string(probe)
	rotation, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0", "-show_entries",
		"stream_side_data=rotation", "-of", "default=noprint_wrappers=1", original).Output()
	if err != nil || !strings.Contains(string(rotation), "rotation=") {
		t.Fatalf("the test video's rotation: %v %s", err, rotation)
	}
	for _, want := range []string{"codec_name=h264", "nb_read_packets=1", strings.TrimSpace(string(rotation))} {
		if !strings.Contains(got, want) {
			t.Errorf("the one-frame video lacks %s:\n%s", want, got)
		}
	}
	if info, _ := os.Stat(out); info.Size() >= int64(len(video)) {
		t.Errorf("the one-frame video (%d bytes) isn't smaller than the video (%d)", info.Size(), len(video))
	}
	if _, err := previews.FirstFrameVideo(context.Background(), Item{}, original, t.TempDir()); err != ErrNoVideoFrame {
		t.Errorf("a file that isn't a video: %v", err)
	}
}
