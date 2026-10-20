package media

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	sum, err := hashFile(tmp.Name())
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

// AddFolders adds folders the file was found in to its sidecar's facts (those it lists already, ignoring
// case, are left out). Only the sidecar is rewritten; the original is never touched.
func (s *Store) AddFolders(sha string, folders []string) (changed bool, err error) {
	err = s.Turn(func() error {
		var err error
		changed, err = s.addFolders(sha, folders)
		return err
	})
	return changed, err
}

func (s *Store) addFolders(sha string, folders []string) (changed bool, err error) {
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
	s.wrote(item)
	return true, nil
}

// SetNudity writes a nudity scan's answer into the file's sidecar, replacing an earlier one. Only the sidecar is
// rewritten; the original is never touched. The file comes again in the change feed (see ChangedAt), so
// ScannedAt is set to now, during the turn: a device's feed position can't have passed it.
func (s *Store) SetNudity(sha string, detection Detection) error {
	return s.Turn(func() error {
		detection.ScannedAt = s.now().UTC()
		return s.setNudity(sha, detection)
	})
}

func (s *Store) setNudity(sha string, detection Detection) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[sha]
	if !ok {
		return fmt.Errorf("%s isn't in the data folder", sha)
	}
	sidecar := item.Sidecar
	detected := Detected{}
	if sidecar.Detected != nil {
		detected = *sidecar.Detected
	}
	detected.Nudity = &detection
	sidecar.Detected = &detected
	data, err := json.MarshalIndent(sidecar, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(s.root, filepath.FromSlash(item.Path))+".json", data); err != nil {
		return err
	}
	item.Sidecar = sidecar
	s.wrote(item)
	return nil
}

// Reload reads one original's sidecar from disk again and indexes it: a file another process placed,
// or a sidecar it changed, since this store was opened (path relative to the data folder, as Item.Path).
func (s *Store) Reload(path string) error {
	data, err := os.ReadFile(filepath.Join(s.root, filepath.FromSlash(path)) + ".json")
	if err != nil {
		return err
	}
	var sidecar Sidecar
	if err := json.Unmarshal(data, &sidecar); err != nil {
		return fmt.Errorf("%s.json: %w", path, err)
	}
	if !IsSha256(sidecar.Sha256) {
		return fmt.Errorf("%s.json has no sha256", path)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.items[sidecar.Sha256]; ok && existing.Path != path {
		return nil // a second copy; the first stays, as when the store is opened
	}
	s.items[sidecar.Sha256] = &Item{Path: path, Sidecar: sidecar}
	return nil
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
