package localindex

import (
	"encoding/base64"
	"encoding/json"

	"github.com/journal/server-go/media"
)

// cursor is how far a phone has an index: where in the cache file the next record begins, the record before it (so
// a file started over since is noticed), the time from which files whose sidecar changed are looked at again, and
// the view it was given out for. It is opaque to phones: base64url of JSON.
type cursor struct {
	V     int    `json:"v"`
	Model string `json:"m"`
	Dim   int    `json:"d,omitempty"`
	// Off is where the next record begins; Last where the one before it began, and Sha whose it is (Last 0: none).
	Off  int64  `json:"o"`
	Last int64  `json:"l,omitempty"`
	Sha  string `json:"s,omitempty"`
	// T is when the phone's last look began (Unix nanoseconds): a file whose sidecar changed since (marked private,
	// scored by the nudity scan) is sent again, or taken back.
	T int64        `json:"t"`
	W bool         `json:"w,omitempty"`
	H media.Hidden `json:"h"`
}

const cursorVersion = 1

func (c cursor) view() media.View { return media.View{WithholdPrivate: c.W, Hidden: c.H} }

func (c cursor) encode() string {
	c.V = cursorVersion
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeCursor reads a cursor; ok is false for "" and for one this server can't read (the phone starts over).
func decodeCursor(s string) (c cursor, ok bool) {
	if s == "" {
		return cursor{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || json.Unmarshal(raw, &c) != nil || c.V != cursorVersion || c.Off < 0 || c.Last < 0 || c.Last >= c.Off && c.Sha != "" {
		return cursor{}, false
	}
	return c, true
}
