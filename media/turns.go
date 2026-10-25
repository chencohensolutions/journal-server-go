package media

import (
	"bytes"
	"encoding/json"
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
	written []Item     // the files placed and sidecars rewritten in the turn in progress, for onWrite

	journalMu sync.Mutex
	offset    int64        // how much of the journal this store has read
	onNew     func(Item)   // see OnCatchUp
	onWrite   func([]Item) // see OnWrite
}

func (s *Store) lockPath() string    { return filepath.Join(s.root, ".cache", "write.lock") }
func (s *Store) journalPath() string { return filepath.Join(s.root, ".cache", "write-journal.tsv") }

// OnCatchUp is called with each file another process placed, as this store learns of it.
func (s *Store) OnCatchUp(fn func(Item)) {
	s.turns.journalMu.Lock()
	s.turns.onNew = fn
	s.turns.journalMu.Unlock()
}

// OnWrite is called after each of this process's turns with the files it placed and the sidecars it rewrote (none
// for a turn that wrote only people.json or settings.json), and as this store learns of other processes' writes,
// with those files. It only reads: the copies (package copies) use it to copy a change soon after it is made. fn
// must not call the store, and should return at once.
func (s *Store) OnWrite(fn func([]Item)) {
	s.turns.journalMu.Lock()
	s.turns.onWrite = fn
	s.turns.journalMu.Unlock()
}

func (s *Store) writeHook() func([]Item) {
	s.turns.journalMu.Lock()
	defer s.turns.journalMu.Unlock()
	return s.turns.onWrite
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
	s.turns.written = s.turns.written[:0]
	err = fn()
	if len(s.turns.pending) > 0 {
		if jerr := s.appendJournal(strings.Join(s.turns.pending, "")); jerr != nil && err == nil {
			err = jerr
		}
	}
	if hook := s.writeHook(); hook != nil {
		hook(append([]Item(nil), s.turns.written...))
	}
	return err
}

// wrote notes, during a turn, a file placed or a sidecar rewritten, for the other processes.
func (s *Store) wrote(item *Item) {
	s.turns.pending = append(s.turns.pending, fmt.Sprintf("%s\t%s\n", item.Sidecar.Sha256, item.Path))
	s.turns.written = append(s.turns.written, *item)
	s.indexStale.Store(true)
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
	var written []Item
	for line := range strings.SplitSeq(string(data[:end]), "\n") {
		sha, path, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		_, known := s.Get(sha)
		if err := s.Reload(path); err != nil {
			log.Printf("media: %s: %v", s.journalPath(), err)
			continue
		}
		item, ok := s.Get(sha)
		if ok && !known && s.turns.onNew != nil {
			s.turns.onNew(item)
		}
		if ok {
			written = append(written, item)
		}
	}
	if len(written) > 0 && s.turns.onWrite != nil {
		s.turns.onWrite(written)
	}
}

// startJournal is where this store starts reading the journal: its end before the folder is scanned, so
// nothing written meanwhile is missed (reading a line twice is harmless).
func (s *Store) startJournal() {
	if info, err := os.Stat(s.journalPath()); err == nil {
		s.turns.offset = info.Size()
	}
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
	s.indexItem(&Item{Path: path, Sidecar: sidecar})
	return nil
}
