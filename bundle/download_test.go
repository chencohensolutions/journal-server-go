package bundle

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// MSYS2's packages are tar archives compressed with zstd, with links between files and paths that must stay
// inside the folder they are unpacked to.
func TestExtractTarZstd(t *testing.T) {
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	add := func(h *tar.Header, body string) {
		h.Size = int64(len(body))
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add(&tar.Header{Name: "ucrt64/", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	add(&tar.Header{Name: "ucrt64/bin/libvips-42.dll", Typeflag: tar.TypeReg}, "dll")
	add(&tar.Header{Name: "ucrt64/bin/libvips.dll", Typeflag: tar.TypeSymlink, Linkname: "libvips-42.dll"}, "")
	add(&tar.Header{Name: "../escape.txt", Typeflag: tar.TypeReg}, "no")
	add(&tar.Header{Name: ".PKGINFO", Typeflag: tar.TypeReg}, "pkgname")
	tw.Close()
	zw.Close()

	dir := t.TempDir()
	archive := filepath.Join(dir, "p.pkg.tar.zst")
	if err := os.WriteFile(archive, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := extractTar(archive, out); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"ucrt64/bin/libvips-42.dll": "dll", "ucrt64/bin/libvips.dll": "dll", ".PKGINFO": "pkgname"} {
		got, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", name, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "escape.txt")); err == nil {
		t.Error("a path outside the folder was written")
	}
}

// Sources are added to a folder's list, keeping the files it already named.
func TestWriteSourcesKeepsTheOthers(t *testing.T) {
	dir := t.TempDir()
	if err := writeSources(dir, map[string]Source{"a.onnx": {URL: "https://example/a", Sha256: "aa"}}); err != nil {
		t.Fatal(err)
	}
	if err := writeSources(dir, map[string]Source{"b.onnx": {URL: "https://example/b", Sha256: "bb"}}); err != nil {
		t.Fatal(err)
	}
	var got map[string]Source
	data, _ := os.ReadFile(filepath.Join(dir, SourcesFile))
	if err := json.Unmarshal(data, &got); err != nil || len(got) != 2 || got["a.onnx"].URL != "https://example/a" {
		t.Fatalf("%s: %v", data, err)
	}
}
