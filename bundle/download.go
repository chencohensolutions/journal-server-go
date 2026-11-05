// Package bundle downloads the programs, libraries and models the server uses beside itself: ONNX Runtime and the
// indexes' models (ONNX), a portable libvips with libjxl for Windows (Vips), and ffmpeg for Windows (FFmpeg). Each
// file is checked against a sha256 the source publishes (or one kept here), and files already downloaded and intact
// are kept, so running it again only fetches what is missing. Used by `journal-server setup` and the
// scripts/bundle-onnx and scripts/bundle-vips commands.
package bundle

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Logf writes what is being done; log.Printf by default.
var Logf = log.Printf

// cacheDir is where downloads that are unpacked elsewhere (archives) are kept between runs.
func cacheDir(parts ...string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{cache, "journal-server"}, parts...)...), nil
}

// download fetches url into path unless a file with sha256 hash is already there; an empty hash is not checked.
func download(ctx context.Context, url, path, hash string) error {
	if hash != "" {
		if sum, err := fileSha256(path); err == nil && sum == hash {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	Logf("Downloading %s", url)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if strings.HasPrefix(url, "https://api.github.com/") {
		req.Header.Set("Accept", "application/octet-stream") // the file, not its description
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	partial := path + ".partial"
	body := io.Reader(resp.Body)
	if resp.ContentLength > 50<<20 {
		body = &progress{r: resp.Body, total: resp.ContentLength, name: filepath.Base(path), last: time.Now()}
	}
	if err := writeFile(partial, body); err != nil {
		os.Remove(partial)
		return fmt.Errorf("%s: %w", url, err)
	}
	if hash != "" {
		if sum, err := fileSha256(partial); err != nil || sum != hash {
			os.Remove(partial)
			return fmt.Errorf("%s: sha256 does not match", url)
		}
	}
	return os.Rename(partial, path)
}

// progress writes how far a large download is, every ten seconds.
type progress struct {
	r           io.Reader
	done, total int64
	name        string
	last        time.Time
}

func (p *progress) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if time.Since(p.last) >= 10*time.Second {
		p.last = time.Now()
		Logf("  %s: %d%% of %d MB", p.name, p.done*100/p.total, p.total>>20)
	}
	return n, err
}

func getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func writeFile(path string, r io.Reader) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func copyFile(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	f, err := os.Open(from)
	if err != nil {
		return err
	}
	defer f.Close()
	return writeFile(to, f)
}

func fileSha256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractTar unpacks a tar archive, plain or compressed with gzip or zstd (told apart by their first bytes, as
// MSYS2's .pkg.tar.zst packages and its .db list are), into dir. Only folders, regular files and links within dir
// are written; links are written as copies, so the result works on Windows too.
func extractTar(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	magic, _ := br.Peek(4)
	var r io.Reader = br
	switch {
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		gz, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
		defer gz.Close()
		r = gz
	case bytes.Equal(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		zr, err := zstd.NewReader(br)
		if err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
		defer zr.Close()
		r = zr
	}
	t := tar.NewReader(r)
	var links [][2]string // target, link: copied once every file is out
	for {
		h, err := t.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
		to, ok := within(dir, h.Name)
		if !ok {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(to, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
				return err
			}
			if err := writeFile(to, t); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			target := h.Linkname
			if h.Typeflag == tar.TypeSymlink {
				target = filepath.Join(filepath.Dir(h.Name), h.Linkname)
			}
			if from, ok := within(dir, target); ok {
				links = append(links, [2]string{from, to})
			}
		}
	}
	for _, l := range links {
		if err := copyFile(l[0], l[1]); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// within is name (a path in an archive) under dir, unless it would land outside dir.
func within(dir, name string) (string, bool) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.Join(dir, clean), true
}
