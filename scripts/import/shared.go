package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/journal/server-go/media"
)

// Several imports may run at once on one data folder (different source folders, or a cloud import next
// to a folder import). Each keeps the data folder's index in memory, so they take turns at everything
// that writes to it: deciding on a file and placing it, adding folders to a sidecar, the review list.
// The turn is the exclusive lock on .cache/import.lock, and whoever holds it first reads what the
// others wrote since its last turn from .cache/import-journal.tsv ("<sha256>\t<path in the data
// folder>", one line per file placed or sidecar changed), so it decides knowing their files too.
// Reading and hashing, the slow part, still run side by side.
type shared struct {
	mu      sync.Mutex // this process's turn (the file lock is per process)
	lock    *os.File
	journal *os.File
	offset  int64 // how much of the journal this process has read
	writes  []string
}

func openShared(dataFolder string) (*shared, error) {
	cache := filepath.Join(dataFolder, ".cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(cache, "import.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	journal, err := os.OpenFile(filepath.Join(cache, "import-journal.tsv"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		lock.Close()
		return nil, err
	}
	// The store was opened from the sidecars just now: what is in the journal so far is in it.
	end, err := journal.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	return &shared{lock: lock, journal: journal, offset: end}, nil
}

func (s *shared) close() {
	s.journal.Close()
	s.lock.Close()
}

// withTurn runs fn while this process alone writes to the data folder, after catching up with what
// other imports wrote, and then records what fn wrote (see wrote) for them.
func (im *importer) withTurn(fn func()) {
	s := im.shared
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := lockFile(s.lock); err != nil {
		log.Fatalf("waiting for the other imports: %v", err)
	}
	defer unlockFile(s.lock)
	im.catchUp()
	s.writes = s.writes[:0]
	fn()
	if len(s.writes) > 0 {
		if _, err := s.journal.WriteString(strings.Join(s.writes, "")); err != nil {
			log.Fatalf("import journal: %v", err)
		}
		if end, err := s.journal.Seek(0, io.SeekEnd); err == nil {
			s.offset = end // only this process wrote since catchUp, under the lock
		}
	}
}

// wrote notes, during a turn, a file placed or a sidecar changed.
func (im *importer) wrote(item media.Item) {
	im.shared.writes = append(im.shared.writes, fmt.Sprintf("%s\t%s\n", item.Sha256(), item.Path))
}

// catchUp reads the journal lines other imports wrote since this process last looked, and indexes their
// files (and photos' dHash, so near-duplicates are found across imports too).
func (im *importer) catchUp() {
	s := im.shared
	end, err := s.journal.Seek(0, io.SeekEnd)
	if err != nil || end <= s.offset {
		return
	}
	data := make([]byte, end-s.offset)
	if _, err := s.journal.ReadAt(data, s.offset); err != nil {
		log.Printf("import journal: %v", err)
		return
	}
	s.offset = end
	for _, line := range strings.Split(string(data), "\n") {
		sha, path, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		_, known := im.store.Get(sha)
		if err := im.store.Reload(path); err != nil {
			log.Printf("import journal: %v", err)
			continue
		}
		item, ok := im.store.Get(sha)
		if known || !ok || item.Sidecar.Facts.Kind != media.KindPhoto {
			continue
		}
		if h := im.dhashOf(context.Background(), sha, filepath.Join(im.dataFolder, filepath.FromSlash(item.Path))); h != nil {
			facts := item.Sidecar.Facts
			im.mu.Lock()
			im.photos = append(im.photos, photo{hash: *h, sha256: sha, name: facts.FileName, takenAt: facts.TakenAt})
			im.mu.Unlock()
		}
	}
}
