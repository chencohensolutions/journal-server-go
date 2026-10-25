// Package media keeps a user's originals in their data folder: each file byte for byte under
// media/YYYY/YYYY-MM/, with a JSON sidecar next to it. The files are the truth; the index in memory
// is rebuilt from the sidecars whenever a store opens. See docs/media.md.
package media

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// SidecarFormat is the version of the sidecar layout written by this server.
const SidecarFormat = 1

type Kind string

const (
	KindPhoto Kind = "photo"
	KindVideo Kind = "video"
	// A sound recording: a recording of an event, a phone call (Facts.Call) or a WhatsApp voice message.
	KindAudio Kind = "audio"
)

type GeoPoint struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// Facts describe the file. The device sends them with the upload (it already read them from
// MediaStore/PhotoKit); the server adds who uploaded it and when.
type Facts struct {
	FileName   string     `json:"fileName"`
	Kind       Kind       `json:"kind"`
	MimeType   string     `json:"mimeType"`
	Size       int64      `json:"size"`
	TakenAt    *time.Time `json:"takenAt"`
	Width      *int       `json:"width"`
	Height     *int       `json:"height"`
	DurationMs *int64     `json:"durationMs"`
	Location   *GeoPoint  `json:"location"`
	Source     *string    `json:"source"`
	// The app a screenshot was taken in, as its file name says (Screenshot_20240301_030715_Bumble.jpg → "Bumble").
	App *string `json:"app,omitempty"`
	// The photo this file is an edit of.
	VersionOf *string `json:"versionOf"`
	// The file this one belongs with: a Live Photo's video names its photo, a RAW its JPEG.
	PairedWith *string   `json:"pairedWith"`
	UploadedAt time.Time `json:"uploadedAt"`
	UploadedBy string    `json:"uploadedBy"`
	// Where scripts/import found the file on the server's disk; absent on uploads from devices.
	ImportedFrom *string `json:"importedFrom,omitempty"`
	// Every folder scripts/import found this file in, from the searched folder down ("phone DCIM/Flowers"):
	// the albums it was kept in, as clues. Copies found later add theirs.
	Folders []string `json:"folders,omitempty"`
	// Who a phone call was with, as the recorder named the file; set only on calls (audio), and the user may correct it (SetCall).
	Call *Call `json:"call,omitempty"`
	// The file this one was encoded from on import (a WAV kept as FLAC), so importing that file again finds this one.
	EncodedFrom *EncodedFrom `json:"encodedFrom,omitempty"`
	// When the server last corrected a fact clients show (SetSource, SetScreenshotFacts, SetCall), so the file comes again in the change feed.
	CorrectedAt *time.Time `json:"correctedAt,omitempty"`
}

// Call is who a recorded phone call was with. Recorders name the file after the contact or, without one, the
// number, so usually only one of the two is known.
type Call struct {
	Contact *string `json:"contact,omitempty"`
	Number  *string `json:"number,omitempty"`
}

// EncodedFrom is the original an imported file was encoded from, which is not kept.
type EncodedFrom struct {
	Sha256   string `json:"sha256"`
	MimeType string `json:"mimeType"`
	Size     int64  `json:"size"`
}

// EndedAt is when a video or recording ended: its start plus its length. Nil without both.
func (f Facts) EndedAt() *time.Time {
	if f.TakenAt == nil || f.DurationMs == nil {
		return nil
	}
	end := f.TakenAt.Add(time.Duration(*f.DurationMs) * time.Millisecond)
	return &end
}

// Sidecar is the <file>.json next to an original.
type Sidecar struct {
	Format int    `json:"format"`
	Sha256 string `json:"sha256"`
	Facts  Facts  `json:"facts"`
	// The user's choice to keep the file private (SetPrivate); absent until it is first marked.
	Privacy *Privacy `json:"privacy,omitempty"`
	// Caption and tags, written through sync (not built yet). Kept as is when the facts are rewritten.
	Note json.RawMessage `json:"note,omitempty"`
	// What models found in the file (scripts/scan-nudity). Never the user's tags: clients decide what to hide.
	Detected *Detected `json:"detected,omitempty"`
}

// Detected is what models found in a file, one entry per kind of finding.
type Detected struct {
	Nudity *Detection `json:"nudity,omitempty"`
}

// Detection is one model's answer about a file.
type Detection struct {
	// 0 (nothing found) to 1 (sure).
	Score float64 `json:"score"`
	// Which model and version answered ("nudenet-320n"), so a better one can scan again.
	Model     string    `json:"model"`
	ScannedAt time.Time `json:"scannedAt"`
}

// Privacy is whether the user marked a file private. It is kept when they unmark it, so that change has a time
// too and comes in the change feed.
type Privacy struct {
	Private bool      `json:"private"`
	SetAt   time.Time `json:"setAt"`
	// The device that marked it, or the one that uploaded an edit or paired file of a private one.
	SetBy string `json:"setBy,omitempty"`
}

// IsPrivate reports whether the user marked the file private.
func (s Sidecar) IsPrivate() bool { return s.Privacy != nil && s.Privacy.Private }

// ChangedAt is the last time the sidecar changed in a way clients see: its upload, a later scan, a correction or
// a private mark.
func (s Sidecar) ChangedAt() time.Time {
	changed := s.Facts.UploadedAt
	if s.Detected != nil && s.Detected.Nudity != nil && s.Detected.Nudity.ScannedAt.After(changed) {
		changed = s.Detected.Nudity.ScannedAt
	}
	if c := s.Facts.CorrectedAt; c != nil && c.After(changed) {
		changed = *c
	}
	if p := s.Privacy; p != nil && p.SetAt.After(changed) {
		changed = p.SetAt
	}
	return changed
}

// Item is one original known to a store.
type Item struct {
	// Path of the original relative to the data folder, with forward slashes.
	Path    string
	Sidecar Sidecar
}

func (i Item) Sha256() string { return i.Sidecar.Sha256 }

// sidecarPath is where item's sidecar is on disk.
func (s *Store) sidecarPath(item *Item) string {
	return filepath.Join(s.root, filepath.FromSlash(item.Path)) + ".json"
}

// writeSidecar writes a sidecar to path, whole or not at all.
func writeSidecar(path string, sidecar Sidecar) error {
	data, err := json.MarshalIndent(sidecar, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

// rewriteSidecar replaces an indexed original's sidecar, on disk and in the index, and notes it for the
// other processes. Callers hold s.mu, during a turn.
func (s *Store) rewriteSidecar(item *Item, sidecar Sidecar) error {
	if err := writeSidecar(s.sidecarPath(item), sidecar); err != nil {
		return err
	}
	item.Sidecar = sidecar
	s.indexItem(item)
	s.wrote(item)
	return nil
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
	return true, s.rewriteSidecar(item, sidecar)
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
	return s.rewriteSidecar(item, sidecar)
}

// SetSource corrects where the file came from (one of the Source constants) in its sidecar. Only the sidecar is
// rewritten; the original is never touched. Like SetNudity, the file comes again in the change feed: CorrectedAt
// is set to now, during the turn. A file that already has that source is left as it is.
func (s *Store) SetSource(sha, source string) (changed bool, err error) {
	err = s.Turn(func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		item, ok := s.items[sha]
		if !ok {
			return fmt.Errorf("%s isn't in the data folder", sha)
		}
		if old := item.Sidecar.Facts.Source; old != nil && *old == source {
			return nil
		}
		sidecar := item.Sidecar
		now := s.now().UTC()
		sidecar.Facts.Source = &source
		sidecar.Facts.CorrectedAt = &now
		changed = true
		return s.rewriteSidecar(item, sidecar)
	})
	return changed, err
}

// SetCall replaces who a recorded call was with (Facts.Call), and with it makes a sound recording a call. Like
// SetSource, it sets CorrectedAt, so the file comes again in the change feed. A file that isn't a sound recording is
// refused; one already so is left as it is.
func (s *Store) SetCall(sha string, call Call) (item Item, changed bool, err error) {
	for _, v := range []**string{&call.Contact, &call.Number} {
		if *v != nil {
			if t := strings.TrimSpace(**v); t == "" {
				*v = nil
			} else {
				*v = &t
			}
		}
	}
	err = s.Turn(func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		found, ok := s.items[sha]
		if !ok {
			return fmt.Errorf("%s isn't in the data folder", sha)
		}
		if found.Sidecar.Facts.Kind != KindAudio {
			return fmt.Errorf("%s is a %s, not a sound recording", sha, found.Sidecar.Facts.Kind)
		}
		if old := found.Sidecar.Facts.Call; old != nil && equalString(old.Contact, call.Contact) && equalString(old.Number, call.Number) {
			item = *found
			return nil
		}
		sidecar := found.Sidecar
		now := s.now().UTC()
		sidecar.Facts.Call = &call
		sidecar.Facts.CorrectedAt = &now
		changed = true
		if err := s.rewriteSidecar(found, sidecar); err != nil {
			return err
		}
		item = *found
		return nil
	})
	return item, changed, err
}

// EncodedFrom finds the file that was encoded on import from the file with this sha256 (a WAV kept as FLAC).
func (s *Store) EncodedFrom(sha string) (Item, bool) {
	s.CatchUp()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if flac, ok := s.encoded[sha]; ok {
		if item, ok := s.items[flac]; ok {
			return *item, true
		}
	}
	return Item{}, false
}

// SetScreenshotFacts gives a file named like a screenshot the facts a new one gets (source screenshots, and the
// app its name says), for files placed before the server read them from the name. Like SetSource, it rewrites only
// the sidecar and sets CorrectedAt, so the file comes again in the change feed; a file already so is left as it is.
func (s *Store) SetScreenshotFacts(sha string) (changed bool, err error) {
	err = s.Turn(func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		item, ok := s.items[sha]
		if !ok {
			return fmt.Errorf("%s isn't in the data folder", sha)
		}
		sidecar := item.Sidecar
		screenshotFacts(&sidecar.Facts)
		if equalString(sidecar.Facts.Source, item.Sidecar.Facts.Source) && equalString(sidecar.Facts.App, item.Sidecar.Facts.App) {
			return nil
		}
		now := s.now().UTC()
		sidecar.Facts.CorrectedAt = &now
		changed = true
		return s.rewriteSidecar(item, sidecar)
	})
	return changed, err
}

func equalString(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
