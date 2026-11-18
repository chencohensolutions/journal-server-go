package main

import (
	"bufio"
	"github.com/journal/server-go/bundle"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAskedSetupNewServer(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(t.TempDir(), "photos")
	// A wrong choice and a bad name are asked again; the CA and model take the defaults.
	answers := strings.Join([]string{"3", "2", "", "hen cohen", "hen", data, "", "2"}, "\n") + "\n"
	o, err := askSetupQuestions(bufio.NewReader(strings.NewReader(answers)), io.Discard, setupOptions{dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if o.user != "hen" || o.dataFolder != data || !o.newCA || o.importCA != "" || o.moveFrom != "" {
		t.Errorf("got user %q, data %q, newCA %v, importCA %q, moveFrom %q", o.user, o.dataFolder, o.newCA, o.importCA, o.moveFrom)
	}
	if o.model != "large" || !o.modelGiven {
		t.Errorf("model %q (given %v), want large", o.model, o.modelGiven)
	}
}

func TestAskedSetupMove(t *testing.T) {
	old := t.TempDir()
	if err := os.WriteFile(filepath.Join(old, configPath), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A folder without config.json is asked again.
	answers := strings.Join([]string{"1", t.TempDir(), `"` + old + `"`}, "\n") + "\n"
	o, err := askSetupQuestions(bufio.NewReader(strings.NewReader(answers)), io.Discard, setupOptions{dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if o.moveFrom != old || o.user != "" || o.newCA {
		t.Errorf("got moveFrom %q, user %q, newCA %v", o.moveFrom, o.user, o.newCA)
	}
}

func TestAskedSetupImportCA(t *testing.T) {
	caDir := t.TempDir()
	for _, f := range []string{"ca.pem", "ca-key.pem"} {
		if err := os.WriteFile(filepath.Join(caDir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	answers := strings.Join([]string{"2", "dana", filepath.Join(t.TempDir(), "d"), "2", caDir, "1"}, "\n") + "\n"
	o, err := askSetupQuestions(bufio.NewReader(strings.NewReader(answers)), io.Discard, setupOptions{dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if o.importCA != caDir || o.newCA || o.modelGiven {
		t.Errorf("got importCA %q, newCA %v, modelGiven %v", o.importCA, o.newCA, o.modelGiven)
	}
}

func TestAskedSetupStopsWhenNotAnswered(t *testing.T) {
	if _, err := askSetupQuestions(bufio.NewReader(strings.NewReader("2\n")), io.Discard, setupOptions{dir: t.TempDir()}); err == nil {
		t.Error("no error when the questions ran out")
	}
}

func TestShippedLibraries(t *testing.T) {
	dir := t.TempDir()
	if s := bundle.ShippedLibraries(dir); s.Any() || s.OnnxRuntime || s.FFmpeg || s.Vips {
		t.Errorf("a folder without libraries.json has shipped libraries: %+v", s)
	}
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, bundle.LibrariesFile), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"v":1,"onnxruntime":"` + bundle.RuntimeVersion + `","ffmpeg":"` + bundle.FFmpegVersion + `","vips":"msys2"}`)
	if s := bundle.ShippedLibraries(dir); !s.OnnxRuntime || !s.FFmpeg || !s.Vips {
		t.Errorf("the installer's libraries not kept: %+v", s)
	}
	// An updated program wants a newer ONNX Runtime and ffmpeg: those are downloaded, then recorded.
	write(`{"v":1,"onnxruntime":"1.0.0","ffmpeg":"1.0","vips":"msys2"}`)
	s := bundle.ShippedLibraries(dir)
	if s.OnnxRuntime || s.FFmpeg || !s.Vips {
		t.Errorf("older libraries kept: %+v", s)
	}
	if err := s.Keep(); err != nil {
		t.Fatal(err)
	}
	if s := bundle.ShippedLibraries(dir); !s.OnnxRuntime || !s.FFmpeg {
		t.Errorf("not recorded after the download: %+v", s)
	}
	write("{damaged")
	if s := bundle.ShippedLibraries(dir); s.Any() || s.Vips {
		t.Errorf("a damaged libraries.json counted: %+v", s)
	}
}
