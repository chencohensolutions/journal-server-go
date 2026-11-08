package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// MaxNoteSize is the largest note SetNote keeps, in bytes once compacted. A note with a long history of its writing
// is still a few kilobytes; this is only a guard against a device sending something that isn't a note.
const MaxNoteSize = 256 << 10

// ErrNoteNotObject and ErrNoteTooLarge are SetNote's answers to a note it won't keep.
var (
	ErrNoteNotObject = errors.New("a note is a JSON object")
	ErrNoteTooLarge  = fmt.Errorf("a note is at most %d bytes", MaxNoteSize)
)

// Note is what the user wrote about a file and how they arranged it in the apps: a title and a paragraph with their
// earlier versions, tags, crops, Moments sets and the like. The apps own its fields (journal-client-kmp
// docs/design.md "Notes"); the server keeps the object as the apps sent it, without reading it, so a newer app's
// fields are never lost. It is kept when the note is deleted, without content, so that change has a time too and
// comes in the change feed.
type Note struct {
	// The apps' JSON object, compacted; absent once the note was deleted.
	Content json.RawMessage `json:"content,omitempty"`
	SetAt   time.Time       `json:"setAt"`
	// The device that wrote it.
	SetBy string `json:"setBy,omitempty"`
	// What else a sidecar's "note" holds (a newer server's fields, or a note in another shape written by hand or by
	// a tool): written back as it was, so rewriting a sidecar never loses it.
	other map[string]json.RawMessage
	raw   json.RawMessage
}

// noteFields is how Note's own fields are written; a note in another shape has no time, and gets none.
type noteFields struct {
	Content json.RawMessage `json:"content,omitempty"`
	SetAt   *time.Time      `json:"setAt,omitempty"`
	SetBy   string          `json:"setBy,omitempty"`
}

// UnmarshalJSON reads a sidecar's "note", keeping what it doesn't know.
func (n *Note) UnmarshalJSON(data []byte) error {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		// Not an object: kept as it is, and read as no note.
		*n = Note{raw: append(json.RawMessage(nil), data...)}
		return nil
	}
	var known noteFields
	_ = json.Unmarshal(data, &known) // a field of another type reads as missing
	if string(known.Content) == "null" {
		known.Content = nil
	}
	for _, key := range []string{"content", "setAt", "setBy"} {
		delete(all, key)
	}
	if len(all) == 0 {
		all = nil
	}
	*n = Note{Content: known.Content, SetBy: known.SetBy, other: all}
	if known.SetAt != nil {
		n.SetAt = *known.SetAt
	}
	return nil
}

// MarshalJSON writes the note with what UnmarshalJSON kept.
func (n Note) MarshalJSON() ([]byte, error) {
	if n.raw != nil && n.Content == nil && n.SetAt.IsZero() {
		return n.raw, nil
	}
	fields := noteFields{Content: n.Content, SetBy: n.SetBy}
	if !n.SetAt.IsZero() {
		fields.SetAt = &n.SetAt
	}
	known, err := json.Marshal(fields)
	if err != nil || len(n.other) == 0 {
		return known, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(known, &all); err != nil {
		return nil, err
	}
	for key, value := range n.other {
		all[key] = value
	}
	return json.Marshal(all)
}

// NoteVersion is the version of a sidecar's note that SetNote compares a device's base with: the sha256 of its
// content, in hex. Empty when there is no note (never written, or deleted).
func (s Sidecar) NoteVersion() string {
	if s.Note == nil || len(s.Note.Content) == 0 {
		return ""
	}
	return noteVersion(s.Note.Content)
}

func noteVersion(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// NoteContent is the note's JSON object, or nil without one.
func (s Sidecar) NoteContent() json.RawMessage {
	if s.Note == nil || len(s.Note.Content) == 0 {
		return nil
	}
	return s.Note.Content
}

// compactNote checks that content is a JSON object of at most MaxNoteSize bytes and compacts it, so the version
// doesn't depend on how a device spaced it. Nil, empty or JSON null is no note (nil).
func compactNote(content []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil, ErrNoteNotObject
	}
	var out bytes.Buffer
	if err := json.Compact(&out, trimmed); err != nil {
		return nil, ErrNoteNotObject
	}
	if out.Len() > MaxNoteSize {
		return nil, ErrNoteTooLarge
	}
	if bytes.Equal(out.Bytes(), []byte("{}")) {
		return nil, nil
	}
	return out.Bytes(), nil
}

// SetNote replaces the note of the file sha (a JPEG kept as JPEG XL is found by either sha256) with content, a JSON
// object; nil or an empty object deletes it. base is the version of the note the device changed (NoteVersion; empty
// for none): when the file's note is no longer that one, nothing is written and saved is false, and the device merges
// its change into the note returned (item) and tries again. Only the sidecar is rewritten, and the file comes again
// in the change feed. Writing the note it already has changes nothing.
func (s *Store) SetNote(sha string, content []byte, base, by string) (item Item, saved bool, err error) {
	compact, err := compactNote(content)
	if err != nil {
		return Item{}, false, err
	}
	err = s.Turn(func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		found, ok := s.find(sha)
		if !ok {
			return fmt.Errorf("%s isn't in the data folder", sha)
		}
		current := found.Sidecar.NoteVersion()
		if current != base {
			item = *found
			return nil
		}
		saved = true
		wanted := ""
		if compact != nil {
			wanted = noteVersion(compact)
		}
		if wanted == current {
			item = *found
			return nil
		}
		sidecar := found.Sidecar
		note := Note{}
		if sidecar.Note != nil {
			note = *sidecar.Note
		}
		note.Content, note.SetAt, note.SetBy, note.raw = compact, s.now().UTC(), by, nil
		sidecar.Note = &note
		if err := s.rewriteSidecar(found, sidecar); err != nil {
			return err
		}
		item = *found
		return nil
	})
	return item, saved, err
}
