package copies

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/journal/server-go/media"
)

// Target is where a copy is kept: a folder (Folder) today, a friend's server later (docs/plans in the client repo,
// "swap"). The copier decides what to copy and when; a target only stores files and gives them back. Paths are
// relative to the copy's root, with forward slashes, as in the data folder.
type Target interface {
	// Ready reports why the copy can't be written to now (ErrUnreachable), or nil.
	Ready() error
	// Stat is the file at path; an error wrapping fs.ErrNotExist when there is none.
	Stat(path string) (File, error)
	// List is the files (not folders) directly in dir; none when dir doesn't exist.
	List(dir string) ([]File, error)
	// Walk calls fn with each file under dir, in no set order; nothing when dir doesn't exist.
	Walk(dir string, fn func(File) error) error
	// Open reads the file at path.
	Open(path string) (io.ReadCloser, error)
	// Put writes size bytes from r to path, whole or not at all: the bytes must have the sha256 sum, or nothing
	// changes. The file then has modTime.
	Put(path string, r io.Reader, size int64, sum string, modTime time.Time) error
	// Touch sets the modified time of the file at path.
	Touch(path string, modTime time.Time) error
	// Remove deletes the file at path; a missing one is an error wrapping fs.ErrNotExist.
	Remove(path string) error
	// Move puts the file at from at to (a previous version kept in history/), replacing nothing.
	Move(from, to string) error
	// Free is how many bytes can still be written beside path.
	Free(path string) (uint64, error)
}

// File is a file in a copy.
type File struct {
	Path    string // relative, with forward slashes
	Size    int64
	ModTime time.Time
}

// Folder is a copy in a folder: another disk, a USB disk, a network share.
type Folder string

func (f Folder) abs(path string) string { return filepath.Join(string(f), filepath.FromSlash(path)) }

func (f Folder) Ready() error {
	info, err := os.Stat(string(f))
	if err != nil {
		return fmt.Errorf("%w (disk unplugged?): %w", ErrUnreachable, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a folder", ErrUnreachable, string(f))
	}
	return nil
}

func (f Folder) Stat(path string) (File, error) {
	info, err := os.Stat(f.abs(path))
	if err != nil {
		return File{}, err
	}
	return File{Path: path, Size: info.Size(), ModTime: info.ModTime()}, nil
}

func (f Folder) List(dir string) ([]File, error) {
	entries, err := os.ReadDir(f.abs(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []File
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		file := File{Path: dir + "/" + e.Name()}
		if info, err := e.Info(); err == nil {
			file.Size, file.ModTime = info.Size(), info.ModTime()
		}
		files = append(files, file)
	}
	return files, nil
}

func (f Folder) Walk(dir string, fn func(File) error) error {
	return filepath.WalkDir(f.abs(dir), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(string(f), path)
		if err != nil {
			return err
		}
		file := File{Path: filepath.ToSlash(rel)}
		if info, err := d.Info(); err == nil {
			file.Size, file.ModTime = info.Size(), info.ModTime()
		}
		return fn(file)
	})
}

func (f Folder) Open(path string) (io.ReadCloser, error) { return os.Open(f.abs(path)) }

// Put writes through a temp file beside path (a dot name, which a compare skips), checks the bytes against sum and
// only then renames it into place.
func (f Folder) Put(path string, r io.Reader, size int64, sum string, modTime time.Time) error {
	dest := f.abs(path)
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dest)+".*.copying")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // already moved away once in place
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), r)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != sum || n != size {
		return fmt.Errorf("%w: %d bytes with sha256 %s, not %d with %s", errDamaged, n, got, size, sum)
	}
	if err := media.Rename(tmp.Name(), dest); err != nil {
		return err
	}
	os.Chtimes(dest, modTime, modTime)
	return nil
}

func (f Folder) Touch(path string, modTime time.Time) error {
	return os.Chtimes(f.abs(path), modTime, modTime)
}

func (f Folder) Remove(path string) error { return os.Remove(f.abs(path)) }

func (f Folder) Move(from, to string) error {
	if _, err := os.Stat(f.abs(to)); err == nil {
		return fmt.Errorf("%s is there already", to)
	}
	if err := os.MkdirAll(filepath.Dir(f.abs(to)), 0o755); err != nil {
		return err
	}
	return os.Rename(f.abs(from), f.abs(to))
}

// Free asks the disk of path's nearest folder that exists (path's own may not be made yet).
func (f Folder) Free(path string) (uint64, error) {
	dir := filepath.Dir(f.abs(path))
	for {
		if _, err := os.Stat(dir); err == nil {
			return media.FreeBytes(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return media.FreeBytes(dir)
		}
		dir = parent
	}
}

// errDamaged is a Put whose bytes aren't the ones it was told: the source changed or is damaged.
var errDamaged = errors.New("the bytes read aren't the file's")

// readAll reads a whole (small) file from t.
func readAll(t Target, path string) ([]byte, error) {
	r, err := t.Open(path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// hashOf is the sha256 and size of a file in t.
func hashOf(t Target, path string) (string, int64, error) {
	r, err := t.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer r.Close()
	h := sha256.New()
	n, err := io.Copy(h, r)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// isTemp is a name the copier writes only for a moment (a Put's temp file) or for itself (savedName).
func isTemp(name string) bool { return strings.HasPrefix(name, ".") }
