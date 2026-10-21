package media

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Import copies a file from elsewhere on the server's disk into the store, as if a device had uploaded
// it (scripts/import). req.Sha256 is the source's hash, which the caller read; the copy is hashed again
// before it is placed. A file the store already has is not copied: its item comes back with existed set.
// The source is never changed.
func (s *Store) Import(source string, req UploadRequest) (item Item, existed bool, err error) {
	if err := req.validate(); err != nil {
		return Item{}, false, err
	}
	unlock := s.uploadLocks.Lock(req.Sha256)
	defer unlock()

	s.CatchUp()
	if existing, ok := s.Get(req.Sha256); ok {
		return existing, true, nil
	}
	if req.Size > MaxFileSize {
		return Item{}, false, fmt.Errorf("larger than %d GB", MaxFileSize>>30)
	}
	if free, err := freeBytes(s.root); err == nil && free < uint64(req.Size)+spaceReserve {
		return Item{}, false, fmt.Errorf("not enough space left in %s", s.root)
	}

	if err := os.MkdirAll(s.importDir(), 0o755); err != nil {
		return Item{}, false, err
	}
	tmp, err := os.CreateTemp(s.importDir(), req.Sha256+".*.tmp")
	if err != nil {
		return Item{}, false, err
	}
	defer os.Remove(tmp.Name()) // already moved away once placed
	if err := copyInto(tmp, source); err != nil {
		tmp.Close()
		return Item{}, false, err
	}
	if err := tmp.Close(); err != nil {
		return Item{}, false, err
	}
	sum, err := HashFile(tmp.Name())
	if err != nil {
		return Item{}, false, err
	}
	if sum != req.Sha256 {
		return Item{}, false, fmt.Errorf("the copy's sha256 is %s, not %s (was the file changed meanwhile?)", sum, req.Sha256)
	}
	// Copied and checked outside the turn; only placing it is in it.
	err = s.Turn(func() error {
		if existing, ok := s.Get(req.Sha256); ok {
			item, existed = existing, true // another process placed it meanwhile
			return nil
		}
		placed, err := s.place(tmp.Name(), req)
		if err == nil {
			item = *placed
		}
		return err
	})
	return item, existed, err
}

// ClearImports deletes copies left behind by an import that was stopped halfway: those not written to for a
// day, as another import may be copying right now.
func (s *Store) ClearImports() error {
	entries, err := os.ReadDir(s.importDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && s.now().Sub(info.ModTime()) > 24*time.Hour {
			os.Remove(filepath.Join(s.importDir(), entry.Name()))
		}
	}
	return nil
}

func (s *Store) importDir() string { return filepath.Join(s.root, ".cache", "import") }

// copyInto copies the file at source into dst and flushes it to disk.
func copyInto(dst *os.File, source string) error {
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer src.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	return dst.Sync()
}
