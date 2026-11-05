package media

import (
	"os"
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
