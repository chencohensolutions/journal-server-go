package bundle

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
)

// The ffmpeg build for Windows: Gyan Doshi's "essentials" build of a release, from its GitHub mirror (the same
// build gyan.dev links to; it has the HEVC decoder, the FLAC encoder and ffprobe). Pinned with the sha256 GitHub
// gives for the file, so a release can't change under the installer.
const (
	FFmpegVersion = "9.0.2"
	ffmpegURL     = "https://github.com/GyanD/codexffmpeg/releases/download/" + FFmpegVersion + "/ffmpeg-" + FFmpegVersion + "-essentials_build.zip"
	ffmpegSha256  = "60f467265b1e312373dbcd92200c2618a74850f98d3d078e94296bb3fa2047ba"
)

// FFmpeg puts ffmpeg.exe and ffprobe.exe for Windows in dir/bin (where media.FindFFmpeg looks), and the licence
// beside them. The server makes video posters with them, and the import reads sound recordings with them.
func FFmpeg(ctx context.Context, dir string) error {
	cache, err := cacheDir("ffmpeg")
	if err != nil {
		return err
	}
	archive := filepath.Join(cache, path.Base(ffmpegURL))
	if err := download(ctx, ffmpegURL, archive, ffmpegSha256); err != nil {
		return err
	}
	z, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer z.Close()
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	wanted := map[string]string{
		"ffmpeg.exe":  filepath.Join(dir, "bin", "ffmpeg.exe"),
		"ffprobe.exe": filepath.Join(dir, "bin", "ffprobe.exe"),
		"LICENSE":     filepath.Join(dir, "LICENSE"),
	}
	n := 0
	for _, f := range z.File {
		to, ok := wanted[path.Base(f.Name)]
		if !ok || f.FileInfo().IsDir() {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return err
		}
		r, err := f.Open()
		if err != nil {
			return err
		}
		err = writeFile(to, r)
		r.Close()
		if err != nil {
			return err
		}
		n++
	}
	if n < 2 {
		return fmt.Errorf("%s: ffmpeg.exe or ffprobe.exe is missing", archive)
	}
	Logf("ffmpeg %s in %s", FFmpegVersion, filepath.Join(dir, "bin"))
	return nil
}
