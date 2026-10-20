package media

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	sum, err := hashFile(tmp.Name())
	if err != nil {
		return Item{}, false, err
	}
	if sum != req.Sha256 {
		return Item{}, false, fmt.Errorf("the copy's sha256 is %s, not %s (was the file changed meanwhile?)", sum, req.Sha256)
	}
	placed, err := s.place(tmp.Name(), req)
	if err != nil {
		return Item{}, false, err
	}
	return *placed, false, nil
}

// AddFolders adds folders the file was found in to its sidecar's facts (those it lists already, ignoring
// case, are left out). Only the sidecar is rewritten; the original is never touched.
func (s *Store) AddFolders(sha string, folders []string) (changed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[sha]
	if !ok {
		return false, fmt.Errorf("%s isn't in the data folder", sha)
	}
	sidecar := item.Sidecar
	known := map[string]bool{}
	for _, f := range sidecar.Facts.Folders {
		known[strings.ToLower(f)] = true
	}
	merged := append([]string(nil), sidecar.Facts.Folders...)
	for _, f := range folders {
		if f != "" && !known[strings.ToLower(f)] {
			known[strings.ToLower(f)] = true
			merged = append(merged, f)
		}
	}
	if len(merged) == len(sidecar.Facts.Folders) {
		return false, nil
	}
	sidecar.Facts.Folders = merged
	data, err := json.MarshalIndent(sidecar, "", "  ")
	if err != nil {
		return false, err
	}
	if err := writeFileAtomic(filepath.Join(s.root, filepath.FromSlash(item.Path))+".json", data); err != nil {
		return false, err
	}
	item.Sidecar = sidecar
	return true, nil
}

// ClearImports deletes copies left behind by an import that was stopped halfway.
func (s *Store) ClearImports() error { return os.RemoveAll(s.importDir()) }

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
