package media

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
