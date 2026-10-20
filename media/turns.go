package media

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Several processes may have the same data folder open at once: the server, scripts/import (one or
// more), scripts/scan-nudity. Each keeps the index in memory, so they take turns at the few steps that
// change it (placing a file, rewriting a sidecar), holding the exclusive lock on .cache/write.lock. Each
// turn first catches up with what the others did, from .cache/write-journal.tsv ("<sha256>\t<path>" per
// file placed or sidecar rewritten), and appends its own. A turn takes milliseconds: copying and hashing
// happen before it. Outside turns, a store catches up before answering what a device asks about the whole
// folder (Missing, Changes, Usage), so a file another process placed is never missed.
type turns struct {
	mu      sync.Mutex // this process's turn; the file lock is per process
	pending []string   // journal lines of the turn in progress

	journalMu sync.Mutex
	offset    int64      // how much of the journal this store has read
	onNew     func(Item) // see OnCatchUp
}

func (s *Store) lockPath() string    { return filepath.Join(s.root, ".cache", "write.lock") }
func (s *Store) journalPath() string { return filepath.Join(s.root, ".cache", "write-journal.tsv") }

// OnCatchUp is called with each file another process placed, as this store learns of it.
func (s *Store) OnCatchUp(fn func(Item)) {
	s.turns.journalMu.Lock()
	s.turns.onNew = fn
	s.turns.journalMu.Unlock()
}

// Turn runs fn while this process alone changes the data folder, after catching up with the others. The
// store's own writes (Import, AddFolders, SetNudity, uploads) take their turn themselves; Turn is for a
// caller's own files that several processes share, such as scripts/import's review list. Not reentrant.
func (s *Store) Turn(fn func() error) error {
	s.turns.mu.Lock()
	defer s.turns.mu.Unlock()
	lock, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lockFile(lock); err != nil {
		return fmt.Errorf("waiting for the other processes using %s: %w", s.root, err)
	}
	defer unlockFile(lock)

	s.CatchUp()
	s.turns.pending = s.turns.pending[:0]
	err = fn()
	if len(s.turns.pending) > 0 {
		if jerr := s.appendJournal(strings.Join(s.turns.pending, "")); jerr != nil && err == nil {
			err = jerr
		}
	}
	return err
}

// wrote notes, during a turn, a file placed or a sidecar rewritten, for the other processes.
func (s *Store) wrote(item *Item) {
	s.turns.pending = append(s.turns.pending, fmt.Sprintf("%s\t%s\n", item.Sidecar.Sha256, item.Path))
}

func (s *Store) appendJournal(lines string) error {
	s.turns.journalMu.Lock()
	defer s.turns.journalMu.Unlock()
	f, err := os.OpenFile(s.journalPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(lines); err != nil {
		return err
	}
	// Under the lock, right after catching up: these are the journal's only new lines, and this store
	// already knows them.
	if info, err := f.Stat(); err == nil {
		s.turns.offset = info.Size()
	}
	return nil
}

// CatchUp indexes the files other processes placed, and the sidecars they rewrote, since it last looked.
// Cheap when nothing changed (one stat).
func (s *Store) CatchUp() {
	s.turns.journalMu.Lock()
	defer s.turns.journalMu.Unlock()
	info, err := os.Stat(s.journalPath())
	if err != nil || info.Size() <= s.turns.offset {
		return
	}
	f, err := os.Open(s.journalPath())
	if err != nil {
		return
	}
	data := make([]byte, info.Size()-s.turns.offset)
	_, err = f.ReadAt(data, s.turns.offset)
	f.Close()
	if err != nil && err != io.EOF {
		log.Printf("media: %s: %v", s.journalPath(), err)
		return
	}
	// A line still being written (by a process in its turn) is read next time.
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return
	}
	s.turns.offset += int64(end + 1)
	for _, line := range strings.Split(string(data[:end]), "\n") {
		sha, path, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		_, known := s.Get(sha)
		if err := s.Reload(path); err != nil {
			log.Printf("media: %s: %v", s.journalPath(), err)
			continue
		}
		if item, ok := s.Get(sha); ok && !known && s.turns.onNew != nil {
			s.turns.onNew(item)
		}
	}
}

// startJournal is where this store starts reading the journal: its end before the folder is scanned, so
// nothing written meanwhile is missed (reading a line twice is harmless).
func (s *Store) startJournal() {
	if info, err := os.Stat(s.journalPath()); err == nil {
		s.turns.offset = info.Size()
	}
}
