package copies

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"time"
)

// savedName is the copier's own file at the copy folder's root: its last compare and its last full pass, so the apps
// see them as soon as the server starts rather than "comparing" for the minutes a compare takes on a USB disk. It is
// not part of the data folder's layout: a compare walks only media/, and a pass never writes it.
const savedName = ".journal-copy.json"

type saved struct {
	V        int          `json:"v"`
	Compare  *savedReport `json:"compare,omitempty"`
	LastPass *savedPass   `json:"lastPass,omitempty"`
}

type savedReport struct {
	At            time.Time `json:"at"`
	Total         int       `json:"total"`
	Originals     int       `json:"originals"`
	OriginalBytes int64     `json:"originalBytes"`
	Sidecars      int       `json:"sidecars"`
	InCopyOnly    int       `json:"inCopyOnly"`
}

type savedPass struct {
	StartedAt    time.Time `json:"startedAt"`
	FinishedAt   time.Time `json:"finishedAt"`
	Files        int       `json:"files"`
	Bytes        int64     `json:"bytes"`
	Failed       int       `json:"failed"`
	Dropped      int       `json:"dropped"`
	DroppedBytes int64     `json:"droppedBytes"`
	DiskFull     bool      `json:"diskFull,omitempty"`
	Stopped      bool      `json:"stopped,omitempty"`
}

// Load takes the last compare and pass saved in the copy folder, when the copier has none of its own yet. A missing
// or unreadable file leaves the copier as it was: the next compare makes it again.
func (c *Copier) Load() {
	data, err := readAll(c.dest(), savedName)
	if err != nil {
		return
	}
	var s saved
	if err := json.Unmarshal(data, &s); err != nil || s.V != 1 {
		log.Printf("copy %s → %s: %s isn't readable, compared again: %v", c.User, c.Folder, savedName, err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := s.Compare; r != nil && c.compared == nil {
		c.compared = &Compare{At: r.At, Report: Report{Total: r.Total, Originals: r.Originals,
			OriginalBytes: r.OriginalBytes, Sidecars: r.Sidecars, InCopyOnly: r.InCopyOnly}}
	}
	if p := s.LastPass; p != nil && c.last == nil {
		c.last = &Pass{StartedAt: p.StartedAt, FinishedAt: p.FinishedAt, Files: p.Files, Bytes: p.Bytes,
			Failed: p.Failed, Dropped: p.Dropped, DroppedBytes: p.DroppedBytes, Stopped: p.Stopped}
		if p.DiskFull {
			c.last.Err = ErrDiskFull
		}
	}
}

// save writes the last compare and pass into the copy folder (temp file and rename). Failing to is only logged: the
// folder may be out of reach, and the next compare is made anyway.
func (c *Copier) save() {
	c.mu.Lock()
	s := saved{V: 1}
	if r := c.compared; r != nil && !r.At.IsZero() {
		s.Compare = &savedReport{At: r.At, Total: r.Total, Originals: r.Originals, OriginalBytes: r.OriginalBytes,
			Sidecars: r.Sidecars, InCopyOnly: r.InCopyOnly}
	}
	if p := c.last; p != nil {
		s.LastPass = &savedPass{StartedAt: p.StartedAt, FinishedAt: p.FinishedAt, Files: p.Files, Bytes: p.Bytes,
			Failed: p.Failed, Dropped: p.Dropped, DroppedBytes: p.DroppedBytes, DiskFull: errors.Is(p.Err, ErrDiskFull),
			Stopped: p.Stopped}
	}
	c.mu.Unlock()
	if c.ready() != nil {
		return
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err == nil {
		sum := sha256.Sum256(data)
		err = c.dest().Put(savedName, bytes.NewReader(data), int64(len(data)), hex.EncodeToString(sum[:]), c.now())
	}
	if err != nil {
		log.Printf("copy %s → %s: saving %s: %v", c.User, c.Folder, savedName, err)
	}
}
