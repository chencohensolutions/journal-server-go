// Package copies keeps a plain copy of a user's data folder in other folders (another disk, a USB disk, a network
// share): the originals, their sidecars, people.json and settings.json, in the data folder's own layout, so a copy
// can be opened as a data folder. It only reads the data folder; the copy folder itself is the record of what was
// copied, and nothing is deleted from it but a JPEG the data folder keeps as JPEG XL, once the copy's JPEG XL makes
// it byte for byte (jxl.go). See docs/copies.md.
package copies

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/media"
)

// spaceReserve stays free on the copy's disk; a pass stops before eating into it.
const spaceReserve uint64 = 1 << 30

// rootFiles are the files at the data folder's root that are copied beside media/: the user's choices, and the
// journals of fix-sources' relabels and of the server's sound-folder moves (relabels/*.tsv), the only way to undo
// one.
func (c *Copier) rootFiles() []string {
	files := []string{"people.json", "settings.json", "cameras.json", "sound-folders.json"}
	journals, _ := filepath.Glob(c.source(filepath.Join("relabels", "*.tsv")))
	for _, journal := range journals {
		files = append(files, filepath.Join("relabels", filepath.Base(journal)))
	}
	return files
}

// ErrDiskFull stops a pass when the copy's disk has no room for the next original.
var ErrDiskFull = errors.New("the copy's disk is full")

// Copier keeps one copy of one user's data folder.
type Copier struct {
	User     string
	Folder   string
	At       string // the daily pass, "15:04"
	Realtime bool

	store *media.Store
	now   func() time.Time

	passMu sync.Mutex // one pass, or realtime batch, at a time

	mu      sync.Mutex
	running *Progress
	last    *Pass
	queued  map[string]bool // sha256s waiting for the realtime batch
	dropped map[string]bool // JPEGs kept as JPEG XL that this copy has no more (jxl.go)
	root    bool            // the root files wait too
	wake    chan struct{}
}

// New is a copier of store into config's folder.
func New(user string, store *media.Store, config auth.CopyConfig, now func() time.Time) *Copier {
	return &Copier{User: user, Folder: config.Folder, At: config.At, Realtime: config.Realtime, store: store, now: now,
		queued: map[string]bool{}, dropped: map[string]bool{}, wake: make(chan struct{}, 1)}
}

// Progress is a pass under way.
type Progress struct {
	StartedAt time.Time
	Files     int
	Bytes     int64
}

// Pass is how a full pass ended.
type Pass struct {
	StartedAt, FinishedAt time.Time
	Files                 int   // copied
	Bytes                 int64 // copied
	Failed                int   // files that couldn't be copied; the pass went on
	Dropped               int   // JPEGs deleted from the copy once their JPEG XL was in it (jxl.go)
	DroppedBytes          int64 // and their size
	Err                   error // what stopped the pass (folder missing, disk full), or nil
}

// Report is what a copy lacks: what a pass would copy now.
type Report struct {
	Originals     int   // originals not in the copy
	OriginalBytes int64 // and their size
	Sidecars      int   // sidecars, people.json, settings.json that differ from the copy's
	InCopyOnly    int   // originals in the copy the data folder no longer has (kept)
	Paths         []string
}

// Status is what the copier is doing and how its last pass ended (nil when none since the server started).
func (c *Copier) Status() (running *Progress, last *Pass) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running != nil {
		p := *c.running
		running = &p
	}
	if c.last != nil {
		p := *c.last
		last = &p
	}
	return running, last
}

// Ready reports why the copy folder can't be written to now, or nil.
func (c *Copier) Ready() error {
	info, err := os.Stat(c.Folder)
	if err != nil {
		return fmt.Errorf("the folder can't be reached (disk unplugged?): %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a folder", c.Folder)
	}
	return nil
}

// SameDisk reports whether the copy is on the data folder's disk, which a failing disk would take with it.
func (c *Copier) SameDisk() (bool, error) { return sameDisk(c.store.Root(), c.Folder) }

// Run makes a full pass: everything the copy lacks is copied. A file that fails is logged and counted, and the pass
// goes on; a missing folder or a full disk stops it.
func (c *Copier) Run(ctx context.Context) Pass {
	c.passMu.Lock()
	defer c.passMu.Unlock()
	pass := Pass{StartedAt: c.now()}
	c.mu.Lock()
	c.running = &Progress{StartedAt: pass.StartedAt}
	c.mu.Unlock()
	pass.Err = c.Ready()
	if pass.Err == nil {
		pass.Err = c.copy(ctx, c.items(), true, &pass)
	}
	pass.FinishedAt = c.now()
	c.mu.Lock()
	c.running = nil
	c.last = &pass
	c.mu.Unlock()
	return pass
}

// Check is a pass that copies nothing: what the copy lacks now. With paths, it lists them (relative to the data
// folder, with forward slashes).
func (c *Copier) Check(ctx context.Context, paths bool) (Report, error) {
	var report Report
	if err := c.Ready(); err != nil {
		return report, err
	}
	known := map[string]bool{}
	for _, item := range c.items() {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		known[strings.ToLower(item.Path)] = true
		need, err := c.originalNeeded(item)
		if err != nil {
			return report, err
		}
		if need {
			report.Originals++
			report.OriginalBytes += item.Sidecar.Facts.Size
			if paths {
				report.Paths = append(report.Paths, item.Path)
			}
		}
		if need, err := smallNeeded(c.source(item.Path)+".json", c.target(item.Path)+".json"); err != nil {
			return report, err
		} else if need {
			report.Sidecars++
			if paths {
				report.Paths = append(report.Paths, item.Path+".json")
			}
		}
	}
	for _, name := range c.rootFiles() {
		if need, err := smallNeeded(c.source(name), c.target(name)); err != nil {
			return report, err
		} else if need {
			report.Sidecars++
			if paths {
				report.Paths = append(report.Paths, name)
			}
		}
	}
	// Originals in the copy only: files under media/ that aren't sidecars or our temp files.
	err := filepath.WalkDir(c.target("media"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		name := d.Name()
		if d.IsDir() || strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			return nil
		}
		rel, err := filepath.Rel(c.Folder, path)
		if err != nil {
			return err
		}
		if !known[strings.ToLower(filepath.ToSlash(rel))] {
			report.InCopyOnly++
		}
		return nil
	})
	return report, err
}

// Changed queues files a turn wrote for the realtime batch (and the root files, which a turn may have written).
func (c *Copier) Changed(items []media.Item) {
	c.mu.Lock()
	for _, item := range items {
		c.queued[item.Sidecar.Sha256] = true
	}
	c.root = true
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// RealtimeDelay is how long after a change the realtime batch starts, so the files of one upload burst go together.
var RealtimeDelay = 5 * time.Second

// RunRealtime copies what Changed queued, a few seconds after it was queued, until ctx ends. A file that can't be
// copied now (the disk unplugged) waits for the next full pass.
func (c *Copier) RunRealtime(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(RealtimeDelay):
		}
		c.mu.Lock()
		queued, root := c.queued, c.root
		c.queued, c.root = map[string]bool{}, false
		c.mu.Unlock()
		var items []media.Item
		for sha := range queued {
			if item, ok := c.store.Get(sha); ok {
				items = append(items, item)
			}
		}
		c.passMu.Lock()
		var pass Pass
		started := c.now()
		err := c.Ready()
		if err == nil {
			err = c.copy(ctx, items, root, &pass)
		}
		c.passMu.Unlock()
		switch {
		case err != nil:
			log.Printf("copy %s → %s: %d files wait for the next full pass: %v", c.User, c.Folder, len(items), err)
		case pass.Files > 0 || pass.Failed > 0 || pass.Dropped > 0:
			log.Printf("copy %s → %s: %d files, %s in %s, %d failed, %d JPEGs kept as JPEG XL deleted", c.User, c.Folder,
				pass.Files, Size(pass.Bytes), c.now().Sub(started).Round(time.Millisecond), pass.Failed, pass.Dropped)
		}
	}
}

// items are the store's originals, in path order.
func (c *Copier) items() []media.Item {
	var items []media.Item
	c.store.Range(func(item media.Item) { items = append(items, item) })
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	return items
}

func (c *Copier) source(path string) string {
	return filepath.Join(c.store.Root(), filepath.FromSlash(path))
}

func (c *Copier) target(path string) string { return filepath.Join(c.Folder, filepath.FromSlash(path)) }

// copy copies what the copy lacks of items and (with root) the root files, adding to pass.
func (c *Copier) copy(ctx context.Context, items []media.Item, root bool, pass *Pass) error {
	counted := func(n int64) {
		pass.Files++
		pass.Bytes += n
		c.mu.Lock()
		if c.running != nil {
			c.running.Files, c.running.Bytes = pass.Files, pass.Bytes
		}
		c.mu.Unlock()
	}
	failed := func(path string, err error) {
		pass.Failed++
		log.Printf("copy %s → %s: %s: %v", c.User, c.Folder, path, err)
	}
	names := map[string][]string{} // the copy's month folders, for dropJPEG
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		need, err := c.originalNeeded(item)
		if err != nil {
			failed(item.Path, err)
			continue
		}
		if need {
			n, err := c.copyOriginal(item)
			if errors.Is(err, ErrDiskFull) {
				return err
			}
			if err != nil {
				failed(item.Path, err)
				continue // without its original, its sidecar waits too
			}
			counted(n)
		}
		if n, copied, err := copySmall(c.source(item.Path)+".json", c.target(item.Path)+".json"); err != nil {
			failed(item.Path+".json", err)
		} else if copied {
			counted(n)
		}
		if item.Sidecar.Facts.IsJXLOfJPEG() {
			if n, err := c.dropJPEG(item, names); err != nil {
				failed(item.Path, err)
			} else if n > 0 {
				pass.Dropped++
				pass.DroppedBytes += n
			}
		}
	}
	if root {
		for _, name := range c.rootFiles() {
			if n, copied, err := copySmall(c.source(name), c.target(name)); err != nil {
				failed(name, err)
			} else if copied {
				counted(n)
			}
		}
	}
	return nil
}

// originalNeeded: an original is in the copy when a file of its size is at its path (originals never change, and
// are written to the copy whole or not at all).
func (c *Copier) originalNeeded(item media.Item) (bool, error) {
	info, err := os.Stat(c.target(item.Path))
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	src, err := os.Stat(c.source(item.Path))
	if err != nil {
		return false, err
	}
	return info.Size() != src.Size(), nil
}

// copyOriginal copies an original through a temp file beside its place in the copy, checks the bytes against its
// sha256 and only then renames it into place, so the copy never holds part of an original.
func (c *Copier) copyOriginal(item media.Item) (int64, error) {
	src, err := os.Open(c.source(item.Path))
	if err != nil {
		return 0, err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return 0, err
	}
	dest := c.target(item.Path)
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	if free, err := media.FreeBytes(dir); err == nil && free < uint64(info.Size())+spaceReserve {
		return 0, fmt.Errorf("%w: %s free, %s needed for %s", ErrDiskFull, Size(int64(free)), Size(info.Size()), item.Path)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dest)+".*.copying")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name()) // already moved away once in place
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), src)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	if sum := hex.EncodeToString(hash.Sum(nil)); sum != item.Sidecar.Sha256 {
		return 0, fmt.Errorf("its bytes have sha256 %s, not %s: the original on the server may be damaged", sum, item.Sidecar.Sha256)
	}
	if err := media.Rename(tmp.Name(), dest); err != nil {
		return 0, err
	}
	os.Chtimes(dest, info.ModTime(), info.ModTime())
	return n, nil
}

// smallNeeded reports whether a small file (a sidecar, people.json, settings.json) differs from the copy's: a
// different size, or a different modified time and different bytes. A missing source needs nothing.
func smallNeeded(source, target string) (bool, error) {
	src, err := os.Stat(source)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	dst, err := os.Stat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if src.Size() != dst.Size() {
		return true, nil
	}
	if src.ModTime().Equal(dst.ModTime()) {
		return false, nil
	}
	// The same size at another time (or a disk that keeps times coarsely, like FAT): compare the bytes.
	a, err := os.ReadFile(source)
	if err != nil {
		return false, err
	}
	b, err := os.ReadFile(target)
	if err != nil {
		return false, err
	}
	if bytes.Equal(a, b) {
		os.Chtimes(target, src.ModTime(), src.ModTime()) // compared by time next time
		return false, nil
	}
	return true, nil
}

// copySmall copies a small file when the copy's differs, whole or not at all, with the source's modified time, so
// the next pass sees them alike without reading them.
func copySmall(source, target string) (int64, bool, error) {
	need, err := smallNeeded(source, target)
	if err != nil || !need {
		return 0, false, err
	}
	info, err := os.Stat(source)
	if err != nil {
		return 0, false, err
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return 0, false, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".*.copying")
	if err != nil {
		return 0, false, err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, false, err
	}
	if err := media.Rename(tmp.Name(), target); err != nil {
		return 0, false, err
	}
	os.Chtimes(target, info.ModTime(), info.ModTime())
	return int64(len(data)), true, nil
}

// Size is bytes for people: "1.4 GB".
func Size(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}
