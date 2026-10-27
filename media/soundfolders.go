package media

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The user's sound folders: which folders sound recordings came from (Call, Voice Recorder, super_recorder…) are
// merged into calls or into recordings. They are kept in sound-folders.json at the data folder's root (beside
// cameras.json), written in a turn by the server only:
//
//	{ "format": 1, "folders": [{ "id": "folder:calls", "as": "call" }] }
//
// It holds only the user's marks. A folder nobody marked keeps its own kind (folder:calls), so nothing moves until
// the user marks one; the sound recordings from a folder marked so are call or recording (SourceOf). See
// docs/media.md, "Sound folders".

// SoundFoldersFormat is the version of sound-folders.json.
const SoundFoldersFormat = 1

// SoundFolderMark is the user's mark on one sound folder: its folder kind's id, and call or recording.
type SoundFolderMark struct {
	ID string `json:"id"`
	As string `json:"as"`
}

type soundFoldersFile struct {
	Format  int               `json:"format"`
	Folders []SoundFolderMark `json:"folders"`
}

// SoundFolderMarks are the user's marks, as SourceOf reads them. The zero value marks nothing.
type SoundFolderMarks struct {
	marks map[string]string // by id, lower case: call or recording
}

// NewSoundFolderMarks is marks read as SourceOf reads them; a later mark of the same folder wins, and a mark that
// is neither call nor recording, or not of a folder kind, is left out.
func NewSoundFolderMarks(marks []SoundFolderMark) SoundFolderMarks {
	m := SoundFolderMarks{marks: map[string]string{}}
	for _, mark := range marks {
		id, as := soundFolderID(mark.ID), NormalizeSource(mark.As)
		if id == "" {
			continue
		}
		if isSoundKind(as) {
			m.marks[id] = as
		} else {
			delete(m.marks, id)
		}
	}
	return m
}

// soundFolderID is the folder kind a sound folder is given by: an id (folder:Calls) or a folder's name (Calls), as
// the server writes it; "" for anything else.
func soundFolderID(id string) string {
	id = strings.TrimSpace(id)
	if FolderName(id) == "" {
		id = FolderSource(id)
	}
	if id = NormalizeSource(id); FolderName(id) == "" {
		return ""
	}
	return id
}

// As is call or recording for a folder the user merged into it, "" for one they didn't (and for "").
func (m SoundFolderMarks) As(id string) string {
	if id == "" || m.marks == nil {
		return ""
	}
	return m.marks[NormalizeSource(id)]
}

// list is the marks, sorted by id.
func (m SoundFolderMarks) list() []SoundFolderMark {
	list := make([]SoundFolderMark, 0, len(m.marks))
	for id, as := range m.marks {
		list = append(list, SoundFolderMark{ID: id, As: as})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list
}

func (s *Store) soundFoldersPath() string { return filepath.Join(s.root, "sound-folders.json") }

// readSoundFolderMarks reads sound-folders.json; a data folder without one marks nothing.
func (s *Store) readSoundFolderMarks() (SoundFolderMarks, error) {
	data, err := os.ReadFile(s.soundFoldersPath())
	if errors.Is(err, fs.ErrNotExist) {
		return SoundFolderMarks{}, nil
	}
	if err != nil {
		return SoundFolderMarks{}, err
	}
	var file soundFoldersFile
	if err := json.Unmarshal(data, &file); err != nil {
		return SoundFolderMarks{}, fmt.Errorf("%s: %w", s.soundFoldersPath(), err)
	}
	if file.Format > SoundFoldersFormat {
		return SoundFolderMarks{}, fmt.Errorf("%s has format %d, newer than this server's %d", s.soundFoldersPath(), file.Format, SoundFoldersFormat)
	}
	return NewSoundFolderMarks(file.Folders), nil
}

// SoundFolderMarks are the user's marks on their sound folders, as read when the store opened and set since.
func (s *Store) SoundFolderMarks() SoundFolderMarks {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.soundMarks
}

// SoundFolder is one folder sound recordings came from (SoundFolderOf): its folder kind, its name as most of its
// files spell it, how many sound recordings came from it and how many of them the import placed, the user's mark
// (call, recording, or "" for none), and how many of its files are still being moved after the mark changed.
type SoundFolder struct {
	ID, Name        string
	Files, Imported int
	As              string
	Pending         int
}

// SoundFolders lists every folder the library's sound recordings came from, most files first; files already merged
// into calls or recordings count under their folder still, and a marked folder with no files left is listed too.
func (s *Store) SoundFolders() []SoundFolder {
	s.CatchUp()
	s.mu.RLock()
	defer s.mu.RUnlock()
	byID := map[string]*SoundFolder{}
	names := folderNames{}
	for _, item := range s.items {
		facts := item.Sidecar.Facts
		id := SoundFolderOf(facts)
		if id == "" {
			continue
		}
		f := byID[id]
		if f == nil {
			f = &SoundFolder{ID: id}
			byID[id] = f
		}
		f.Files++
		if facts.Imported() {
			f.Imported++
		}
		names.add(id, folderSpelling(facts, id))
	}
	for id := range s.soundMarks.marks {
		if byID[id] == nil {
			byID[id] = &SoundFolder{ID: id}
		}
	}
	folders := make([]SoundFolder, 0, len(byID))
	for id, f := range byID {
		f.Name = names.name(id)
		f.As = s.soundMarks.As(id)
		f.Pending = s.soundMoves.pendingOf(id)
		folders = append(folders, *f)
	}
	sort.Slice(folders, func(i, j int) bool {
		if folders[i].Files != folders[j].Files {
			return folders[i].Files > folders[j].Files
		}
		return folders[i].ID < folders[j].ID
	})
	return folders
}

// SetSoundFolder merges a sound folder (its folder kind's id, or its name) into calls (as call) or recordings (as
// recording), or takes the mark back (as ""), in sound-folders.json (inside a turn), and answers at once: the
// folder's sound recordings then move in the background, a turn each, to what SourceOf now gives them (call or
// recording, or back to what they had before), and each comes again in the change feed once. Each move is written
// first to a journal, relabels/<UTC time>-sound-folders.tsv, which `fix-sources --undo` undoes. SoundFolders says
// how many are still to move (Pending). A mark changed while a move runs is applied after it.
func (s *Store) SetSoundFolder(id, as string) error {
	key := soundFolderID(id)
	if key == "" {
		return fmt.Errorf("%q isn't a folder", id)
	}
	if as = NormalizeSource(as); as != "" && !isSoundKind(as) {
		return fmt.Errorf("a sound folder is merged into call or recording, not %q", as)
	}
	err := s.Turn(func() error {
		s.mu.RLock()
		marks := s.soundMarks.list()
		s.mu.RUnlock()
		marks = append(marks, SoundFolderMark{ID: key, As: as})
		next := NewSoundFolderMarks(marks)
		data, err := json.MarshalIndent(soundFoldersFile{Format: SoundFoldersFormat, Folders: next.list()}, "", "  ")
		if err != nil {
			return err
		}
		if err := writeFileAtomic(s.soundFoldersPath(), append(data, '\n')); err != nil {
			return err
		}
		s.mu.Lock()
		s.soundMarks = next
		s.mu.Unlock()
		return nil
	})
	if err != nil {
		return err
	}
	mark := as
	if mark == "" {
		mark = "none"
	}
	j := &relabelJournal{root: s.root, what: "sound-folders", about: "setSoundFolder " + key + " as " + mark, now: s.now}
	s.soundMoves.queue(key, len(s.soundFolderMoving(key)))
	go func() {
		s.soundMoves.do(key, func() []relabelMove { return s.soundFolderMoving(key) }, func(m relabelMove) error {
			_, err := s.SetLabels(m.sha, func(disk Labels) (Labels, bool, error) {
				if disk.Source == m.source {
					return disk, false, nil
				}
				next := disk
				next.Source = m.source
				return next, true, j.write(m.sha, disk.Source, m.source)
			})
			return err
		}, "a sound folder's mark changed")
		if err := j.close(); err != nil {
			log.Printf("media: closing %s: %v", j.path, err)
		}
	}()
	return nil
}

// soundFolderMoving lists the sound recordings from the folder key whose source changes with the marks as they are
// now.
func (s *Store) soundFolderMoving(key string) []relabelMove {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var moves []relabelMove
	var kinds *FolderKinds // counted only when a file of the folder is found
	for _, item := range s.items {
		facts := item.Sidecar.Facts
		if SoundFolderOf(facts) != key {
			continue
		}
		current := ""
		if facts.Source != nil {
			current = *facts.Source
		}
		if kinds == nil {
			kinds = s.folderKindsLocked()
		}
		if target, _ := SourceOf(facts, s.cameraMarks, kinds); !SameSource(target, current) {
			moves = append(moves, relabelMove{sha: item.Sha256(), source: target})
		}
	}
	return moves
}

// WaitSoundFolderMoves waits until the moves SetSoundFolder started are done.
func (s *Store) WaitSoundFolderMoves() { s.soundMoves.wait() }
