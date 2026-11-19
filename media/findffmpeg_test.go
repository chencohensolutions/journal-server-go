package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// journal-server setup puts ffmpeg and ffprobe in ffmpeg/bin beside the server; that folder comes before the PATH.
func TestFindFFmpegInSetupFolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs runnable stand-ins; on Windows they would have to be .exe files")
	}
	t.Chdir(t.TempDir())
	bin := filepath.Join("ffmpeg", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ffmpeg, ffprobe, err := FindFFmpeg("")
	if err != nil {
		t.Fatal(err)
	}
	if ffmpeg != filepath.Join(bin, "ffmpeg") || ffprobe != filepath.Join(bin, "ffprobe") {
		t.Errorf("found %s and %s, not the ones in %s", ffmpeg, ffprobe, bin)
	}
}

// ffmpeg and ffprobe read a file from a device as one of the formats phones and cameras write, from this disk only:
// a playlist dressed as a recording, which would have them read other files or addresses, is refused.
func TestFFmpegReadsOnlyKnownFormatsFromThisDisk(t *testing.T) {
	ffmpeg, ffprobe, err := FindFFmpeg("")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "real.m4a")
	if out, err := exec.Command(ffmpeg, "-loglevel", "error", "-f", "lavfi", "-i", "sine=d=1", real).CombinedOutput(); err != nil {
		t.Fatalf("making a recording: %v %s", err, out)
	}
	if a := ProbeAudio(context.Background(), ffprobe, real, "real.m4a", "", nil); a.Failed != "" || a.DurationMs == nil {
		t.Fatalf("a real recording: %+v", a)
	}
	playlists := map[string]string{
		"concat.m4a": "ffconcat version 1.0\nfile real.m4a\n",
		"hls.m4a":    "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\nhttp://127.0.0.1:9/x.ts\n#EXT-X-ENDLIST\n",
	}
	for name, body := range playlists {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if a := ProbeAudio(context.Background(), ffprobe, path, name, "", nil); a.Failed == "" || a.DurationMs != nil {
			t.Errorf("%s was read: %+v", name, a)
		}
		if _, err := EncodeFLAC(context.Background(), ffmpeg, dir, path); err == nil {
			t.Errorf("%s was encoded", name)
		}
	}
}
