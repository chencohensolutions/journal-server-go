package localindex

import (
	"compress/gzip"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/media"
)

// The kinds of frame in a stream (ServeIndex).
const (
	// FrameRecord is a record as the cache file keeps it (a JSON line without its newline). A file that comes
	// again replaces what the phone had of it.
	FrameRecord = 'R'
	// FrameDrop is a file the phone must drop: its sha256's 32 bytes (hidden, or private to a device kept from them).
	FrameDrop = 'D'
	// FrameCheckpoint is a cursor to go on from, should the stream break after it (ASCII).
	FrameCheckpoint = 'C'
	// FrameEnd is the cursor of the next look (ASCII), and the stream's end: a stream without it broke.
	FrameEnd = 'E'
)

// CheckpointEvery is how much of the cache file is read between checkpoints (a variable for tests).
var CheckpointEvery int64 = 4 << 20

// StreamHead is a stream's first line, in JSON.
type StreamHead struct {
	V      int    `json:"v"`
	Index  string `json:"index"`
	Model  string `json:"model"`
	Format string `json:"format"`
	// Dim is the vectors' length (0 for FormatJSONL, and for an index with nothing in it yet).
	Dim int `json:"dim"`
	// Reset: drop everything kept of this index first (another model, a file started over, a cursor this server
	// can't read, or none).
	Reset bool `json:"reset"`
}

// ServeIndex answers GET /index/{name}?cursor=…: the records of the index added since cursor ("" or none: all of
// them), for the device's user, of the files the device may see.
//
// The body is a line of JSON (StreamHead), then frames: a kind byte, the length of what follows as 4 bytes
// little-endian, and that many bytes. First come the files whose visibility changed since the cursor (the user hid
// a collection, a file was marked private, the nudity scan scored one): dropped (FrameDrop) or sent whole
// (FrameRecord, from the index's memory). Then every record the cache file has after the cursor, with a checkpoint
// every few MB. The last frame is FrameEnd, with the cursor to ask with next time. Files deleted from the server are
// not told here: the change feed tells them.
func (s *Server) ServeIndex(w http.ResponseWriter, r *http.Request) {
	src := s.sources[r.PathValue("name")]
	user := auth.FromContext(r.Context())
	if src == nil || user == nil {
		http.NotFound(w, r)
		return
	}
	store := s.library.Store(user.Username)
	if store == nil {
		http.NotFound(w, r)
		return
	}
	view := store.ViewFor(auth.WithholdPrivateFromContext(r.Context()))
	given := r.URL.Query().Get("cursor")
	c, ok := decodeCursor(given)

	file, err := openCache(src.Cache.CacheFile(user.Username), src.Format)
	if err != nil && !errors.Is(err, errNoHeader) {
		log.Printf("localindex: %s for %s: %v", src.Name, user.Username, err)
		http.Error(w, "the index can't be read", http.StatusInternalServerError)
		return
	}
	if file != nil {
		defer file.Close()
	}
	model := src.Cache.Model()
	reset := !ok || c.Model != model
	switch {
	case file == nil:
		// Not read yet, or deleted to be read again: a phone that has some drops it.
		reset = given != ""
	case !reset:
		reset = c.Dim != file.dim || c.Off > file.size || c.Off < file.start ||
			c.Sha != "" && !file.recordAt(c.Last, c.Off, c.Sha)
	}

	w.Header().Set("Content-Type", "application/x-journal-index")
	w.Header().Set("Cache-Control", "no-store")
	var out io.Writer = w
	// The text in photos shrinks several times; vectors are numbers, and hardly do.
	if src.Format == FormatJSONL && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		defer zw.Close()
		out = zw
	}
	st := &stream{w: out}
	head := StreamHead{V: 1, Index: src.Name, Model: model, Format: src.Format, Reset: reset}
	if file != nil {
		head.Dim = file.dim
	}
	line, _ := json.Marshal(head)
	st.write(append(line, '\n'))

	look := time.Now()
	next := cursor{Model: model, T: look.UnixNano(), W: view.WithholdPrivate, H: view.Hidden}
	if file == nil {
		// The next look starts from the beginning, with no cursor.
		st.frame(FrameEnd, nil)
		return
	}
	next.Dim = file.dim
	if reset {
		c = cursor{Off: file.start}
	}
	next.Off, next.Last, next.Sha = c.Off, c.Last, c.Sha

	sent := map[string]bool{}
	if !reset {
		s.changeOver(st, src, store, user.Username, c, view, sent)
	}
	rr := file.reader(c.Off)
	read := int64(0)
	for st.err == nil {
		record, sha, began, err := rr.next()
		if err != nil {
			break
		}
		read += rr.at - began
		next.Off, next.Last, next.Sha = rr.at, began, sha
		if sha != "" && !sent[sha] {
			if item, ok := store.Get(sha); ok && !store.Hides(view, item) {
				st.frame(FrameRecord, record)
			}
		}
		if read >= CheckpointEvery {
			st.frame(FrameCheckpoint, []byte(next.encode()))
			read = 0
		}
	}
	st.frame(FrameEnd, []byte(next.encode()))
	if st.err != nil {
		log.Printf("localindex: %s for %s stopped: %v", src.Name, user.Username, st.err)
	}
}

// changeOver sends what changed for files already sent or left out: with another view than the cursor's, every file
// whose visibility differs between the two; and every file whose sidecar changed since the cursor's look. Each is
// dropped if the device may not see it now, or sent whole (and noted in sent, so the rest of the stream doesn't send
// it again).
func (s *Server) changeOver(st *stream, src *Source, store *media.Store, user string, c cursor, view media.View, sent map[string]bool) {
	old := c.view()
	viewChanged := old.Key() != view.Key()
	var items []media.Item
	store.Range(func(item media.Item) { items = append(items, item) })
	for _, item := range items {
		if st.err != nil {
			return
		}
		hidden := store.Hides(view, item)
		changed := item.Sidecar.ChangedAt().UnixNano() >= c.T
		if !changed && !(viewChanged && store.Hides(old, item) != hidden) {
			continue
		}
		sha := item.Sha256()
		if hidden {
			raw, _ := hex.DecodeString(sha)
			st.frame(FrameDrop, raw)
		} else if record := src.Cache.Record(user, sha); record != nil {
			st.frame(FrameRecord, record)
			sent[sha] = true
		}
	}
}

// stream writes a stream's frames, and keeps the first error (the phone went away).
type stream struct {
	w   io.Writer
	err error
}

func (st *stream) write(b []byte) {
	if st.err == nil {
		_, st.err = st.w.Write(b)
	}
}

func (st *stream) frame(kind byte, payload []byte) {
	head := []byte{kind, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(head[1:], uint32(len(payload)))
	st.write(head)
	st.write(payload)
}
