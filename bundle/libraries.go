package bundle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// LibrariesFile marks a server folder whose libraries came with the program: the Windows installer ships ONNX
// Runtime, libvips and ffmpeg (the Microsoft Store takes only installers that work offline), so journal-server
// setup keeps them and downloads only the models.
const LibrariesFile = "libraries.json"

// librariesRecord is LibrariesFile: which libraries the installer put beside the program.
type librariesRecord struct {
	V           int    `json:"v"`
	OnnxRuntime string `json:"onnxruntime"`
	FFmpeg      string `json:"ffmpeg"`
	Vips        string `json:"vips"`
}

// Libraries puts in dir what the Windows installer ships beside journal-server.exe: ONNX Runtime in onnxruntime\,
// libvips in vips\ and ffmpeg in ffmpeg\bin\, then LibrariesFile. It runs on Windows only (the release builds the
// installer on a Windows runner), since each download is for the platform it runs on.
func Libraries(ctx context.Context, dir string) error {
	if runtime.GOOS != "windows" {
		return errors.New("the installer's libraries are for Windows: run this on Windows")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := Runtime(ctx, filepath.Join(dir, "onnxruntime")); err != nil {
		return fmt.Errorf("onnx runtime: %w", err)
	}
	if err := Vips(ctx, filepath.Join(dir, "vips")); err != nil {
		return fmt.Errorf("libvips: %w", err)
	}
	if err := FFmpeg(ctx, filepath.Join(dir, "ffmpeg")); err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	return writeLibraries(dir, librariesRecord{V: 1, OnnxRuntime: RuntimeVersion, FFmpeg: FFmpegVersion, Vips: "msys2"})
}

func writeLibraries(dir string, r librariesRecord) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, LibrariesFile+".tmp")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, LibrariesFile))
}

// Shipped says which of the libraries in a server folder came with the installer and are the versions this program
// wants, so journal-server setup keeps them. A folder without LibrariesFile (a server set up by downloads, or from a
// git clone) has none; one whose program was updated past its installer's ONNX Runtime or ffmpeg lacks that one, and
// setup downloads it (then calls Keep).
type Shipped struct {
	dir                       string
	record                    librariesRecord
	OnnxRuntime, FFmpeg, Vips bool
}

// ShippedLibraries reads dir's LibrariesFile. A damaged one counts as none: setup then downloads them all.
func ShippedLibraries(dir string) Shipped {
	s := Shipped{dir: dir}
	data, err := os.ReadFile(filepath.Join(dir, LibrariesFile))
	if err != nil || json.Unmarshal(data, &s.record) != nil || s.record.V != 1 {
		return s
	}
	s.OnnxRuntime = s.record.OnnxRuntime == RuntimeVersion
	s.FFmpeg = s.record.FFmpeg == FFmpegVersion
	s.Vips = s.record.Vips != ""
	return s
}

// Any reports whether the folder has a LibrariesFile at all.
func (s Shipped) Any() bool { return s.record.V == 1 }

// Keep records that the folder now has the ONNX Runtime and ffmpeg this program wants (setup downloaded the ones
// that were older), so the next setup keeps them too.
func (s Shipped) Keep() error {
	if !s.Any() || (s.OnnxRuntime && s.FFmpeg) {
		return nil
	}
	r := s.record
	r.OnnxRuntime, r.FFmpeg = RuntimeVersion, FFmpegVersion
	return writeLibraries(s.dir, r)
}
