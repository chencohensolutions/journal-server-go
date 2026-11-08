package importcmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// A dHash is 64 bits, one per pair of neighboring pixels in the photo shrunk to 9×8 and made grey: set
// when the left one is brighter. Photos that look alike (a resized or recompressed copy, a burst) differ
// in a few bits; nearDistance is the most that still counts as a near-duplicate.
type dhash uint64

func (h dhash) distance(o dhash) int { return bits.OnesCount64(uint64(h ^ o)) }

// photoDHash shrinks the photo with libvips (upright, after its EXIF orientation) and hashes it.
func photoDHash(ctx context.Context, vips, workDir, path string) (dhash, error) {
	out, err := os.CreateTemp(workDir, "*.ppm")
	if err != nil {
		return 0, err
	}
	out.Close()
	defer os.Remove(out.Name())
	cmd := exec.CommandContext(ctx, vips, "thumbnail", path, out.Name(), "9", "--height", "8", "--size", "force")
	if msg, err := cmd.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("vips thumbnail: %v: %s", err, strings.TrimSpace(string(msg)))
	}
	data, err := os.ReadFile(out.Name())
	if err != nil {
		return 0, err
	}
	grey, err := readPNM(data, 9, 8)
	if err != nil {
		return 0, err
	}
	var h dhash
	for y := range 8 {
		for x := range 8 {
			h <<= 1
			if grey[y*9+x] > grey[y*9+x+1] {
				h |= 1
			}
		}
	}
	return h, nil
}

// readPNM reads a binary PPM (P6) or PGM (P5) of the given size, as vips writes it (with a comment line),
// and returns its pixels' brightness.
func readPNM(data []byte, width, height int) ([]int, error) {
	var header []string
	rest := data
	for len(header) < 4 {
		rest = bytes.TrimLeft(rest, " \t\r\n")
		if len(rest) > 0 && rest[0] == '#' {
			if i := bytes.IndexByte(rest, '\n'); i >= 0 {
				rest = rest[i+1:]
				continue
			}
			return nil, errors.New("pnm: bad header")
		}
		end := bytes.IndexAny(rest, " \t\r\n")
		if end <= 0 {
			return nil, errors.New("pnm: bad header")
		}
		header = append(header, string(rest[:end]))
		rest = rest[end:]
	}
	rest = rest[1:] // the single whitespace before the pixels
	bands := map[string]int{"P5": 1, "P6": 3}[header[0]]
	w, _ := strconv.Atoi(header[1])
	h, _ := strconv.Atoi(header[2])
	if bands == 0 || w != width || h != height || header[3] != "255" || len(rest) < w*h*bands {
		return nil, fmt.Errorf("pnm: unexpected %s %sx%s %s", header[0], header[1], header[2], header[3])
	}
	grey := make([]int, w*h)
	for i := range grey {
		p := rest[i*bands:]
		if bands == 1 {
			grey[i] = int(p[0]) * 1000
		} else {
			grey[i] = int(p[0])*299 + int(p[1])*587 + int(p[2])*114
		}
	}
	return grey, nil
}

// dhashTable is the dHash of every photo seen, by sha256, kept in the data folder's
// .cache/dhash.tsv ("<sha256>\t<16 hex digits>", or "-" when the photo couldn't be read) so later
// imports don't shrink the same photos again. It is only a cache: deleting it costs a slower next import.
type dhashTable struct {
	mu     sync.Mutex
	hashes map[string]*dhash // nil: couldn't be read
	file   *os.File
}

func openDHashTable(dataFolder string) (*dhashTable, error) {
	path := filepath.Join(dataFolder, ".cache", "dhash.tsv")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	t := &dhashTable{hashes: map[string]*dhash{}}
	if f, err := os.Open(path); err == nil {
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			sha, value, ok := strings.Cut(scanner.Text(), "\t")
			if !ok || len(sha) != 64 {
				continue
			}
			if n, err := strconv.ParseUint(value, 16, 64); err == nil {
				h := dhash(n)
				t.hashes[sha] = &h
			} else {
				t.hashes[sha] = nil
			}
		}
		f.Close()
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	t.file = file
	return t, nil
}

// get returns the photo's dHash, and whether it is known at all (known but nil: couldn't be read).
func (t *dhashTable) get(sha string) (*dhash, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	h, ok := t.hashes[sha]
	return h, ok
}

func (t *dhashTable) put(sha string, h *dhash) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.hashes[sha]; ok {
		return
	}
	t.hashes[sha] = h
	value := "-"
	if h != nil {
		value = fmt.Sprintf("%016x", uint64(*h))
	}
	fmt.Fprintf(t.file, "%s\t%s\n", sha, value)
}

func (t *dhashTable) close() error { return t.file.Close() }
