package media

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// Changes returns up to limit originals changed after cursor ("" for all), oldest change first, with the cursor
// to continue from and how many changes come after this page (none: the feed is read to the end). Originals never
// change, but their sidecars can get a scan's answer later (Sidecar.ChangedAt), so a file comes again once scanned;
// the cursor is opaque to clients, so the feed can later count edits to notes and stories too.
//
// view is what the device may see (View): files it may not see still come, for the caller to send as deleted
// (the device may have them from before they were marked private or hidden), except in a feed from the start, which
// has nothing to take back and leaves them out. The cursor remembers the view it was given out for (whether the
// device was kept from private files, and what the user hid). One given out for another view first sends the files
// whose visibility changed between the two views (to be sent as deleted, or whole), in pages, and then goes on where
// it was; so a device's files are sent or taken back when its setting or the user's hidden collections change,
// without the rest coming again. A cursor the server can't read, or one whose view changed while such a change-over
// was under way, starts the feed again with everything, hidden files included, since what the device holds is
// unknown.
func (s *Store) Changes(cursor string, limit int, view View) (items []Item, next string, remaining int, err error) {
	c := decodeCursor(cursor)
	fresh := cursor == ""
	var old *View             // the view the device had, while its files are changed over to view
	var changed *feedPosition // how far the change-over was sent
	switch {
	case c.key == "": // the start, or a cursor that can't be read: everything (the device may hold anything)
		c.pos = nil
	case c.key == view.key():
		old, changed = c.old, c.changed
	case c.view != nil && c.old == nil:
		old = c.view
	default: // the view changed during a change-over, or an old cursor that names no view: everything again
		c.pos = nil
	}
	s.CatchUp()
	s.mu.RLock()
	var all, over []Item
	for _, item := range s.items {
		if old != nil && !c.pos.before(*item) && changed.before(*item) && s.hides(*old, *item) != s.hides(view, *item) {
			over = append(over, *item)
		}
		if c.pos.before(*item) && !(fresh && s.hides(view, *item)) {
			all = append(all, *item)
		}
	}
	s.mu.RUnlock()
	sort.Slice(over, func(i, j int) bool { return position(over[i]).less(position(over[j])) })
	sort.Slice(all, func(i, j int) bool { return position(all[i]).less(position(all[j])) })
	remaining = len(over) + len(all)
	if len(over) >= limit {
		// The change-over goes on in the next page; the feed's own position stays.
		items = over[:limit]
		last := position(items[limit-1])
		return items, feedCursor{view: &view, pos: c.pos, old: old, changed: &last}.encode(), remaining - limit, nil
	}
	items = append(over, all[:min(len(all), limit-len(over))]...)
	remaining -= len(items)
	pos := c.pos
	if n := len(items) - len(over); n > 0 {
		last := position(items[len(items)-1])
		pos = &last
	}
	if pos == nil {
		return items, "", remaining, nil
	}
	return items, feedCursor{view: &view, pos: pos}.encode(), remaining, nil
}

// feedPosition orders the feed: the time of the last change (upload or scan), then sha256 for files changed in
// the same instant.
type feedPosition struct {
	changedAt int64
	sha       string
}

func position(item Item) feedPosition {
	return feedPosition{item.Sidecar.ChangedAt().UnixNano(), item.Sidecar.Sha256}
}

func (p feedPosition) less(o feedPosition) bool {
	return p.changedAt < o.changedAt || p.changedAt == o.changedAt && p.sha < o.sha
}

// before reports whether item comes after p; the zero position comes before everything.
func (p *feedPosition) before(item Item) bool { return p == nil || p.less(position(item)) }

// feedCursor is what a cursor carries: where the device is in the feed and the view it was given out for; during a
// change-over, also the view the device had and how far the files whose visibility changed were sent.
type feedCursor struct {
	key     string        // the view's key; "" for a cursor that can't be read
	view    *View         // the view itself; nil in cursors from before it was carried
	pos     *feedPosition // nil at the start
	old     *View
	changed *feedPosition
}

// A cursor is base64url of JSON: {"v":3,"w":true,"h":{…},"t":<nanos>,"s":<sha256>}, and during a change-over also
// "o" (the view the device had), "ot" and "os". Cursors from before ("m1:<nanos>:<sha>", "w1:…" while withholding
// private files, "m2:<view key>:<nanos>:<sha>") are still read; they carry only a key of their view, so when it
// changed the feed starts again.
type cursorJSON struct {
	V  int         `json:"v"`
	W  bool        `json:"w,omitempty"`
	H  Hidden      `json:"h"`
	T  int64       `json:"t"`
	S  string      `json:"s"`
	O  *cursorView `json:"o,omitempty"`
	OT int64       `json:"ot,omitempty"`
	OS string      `json:"os,omitempty"`
}

type cursorView struct {
	W bool   `json:"w,omitempty"`
	H Hidden `json:"h"`
}

func (c feedCursor) encode() string {
	j := cursorJSON{V: 3, W: c.view.WithholdPrivate, H: c.view.Hidden, T: c.pos.changedAt, S: c.pos.sha}
	if c.old != nil {
		j.O = &cursorView{W: c.old.WithholdPrivate, H: c.old.Hidden}
		if c.changed != nil {
			j.OT, j.OS = c.changed.changedAt, c.changed.sha
		}
	}
	raw, _ := json.Marshal(j)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeCursor reads a cursor; "" or one the server can't read gives a cursor with no key.
func decodeCursor(cursor string) feedCursor {
	if cursor == "" {
		return feedCursor{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return feedCursor{}
	}
	if strings.HasPrefix(string(raw), "{") {
		return decodeCursorJSON(raw)
	}
	parts := strings.Split(string(raw), ":")
	var key string
	switch {
	case len(parts) == 3 && (parts[0] == "m1" || parts[0] == "w1"):
		key = parts[0][:1]
	case len(parts) == 4 && parts[0] == "m2" && len(parts[1]) > 1 && (parts[1][0] == 'm' || parts[1][0] == 'w'):
		// Two statements: in one, Go may run the append (which writes over parts[1]) before reading parts[1].
		key = parts[1]
		parts = append(parts[:1], parts[2:]...)
	default:
		return feedCursor{}
	}
	pos, ok := parsePosition(parts[1], parts[2])
	if !ok {
		return feedCursor{}
	}
	return feedCursor{key: key, pos: &pos}
}

func decodeCursorJSON(raw []byte) feedCursor {
	var j cursorJSON
	if err := json.Unmarshal(raw, &j); err != nil || j.V != 3 {
		return feedCursor{}
	}
	pos, ok := parsePosition(strconv.FormatInt(j.T, 10), j.S)
	if !ok {
		return feedCursor{}
	}
	view := View{WithholdPrivate: j.W, Hidden: j.H.normalized()}
	c := feedCursor{key: view.key(), view: &view, pos: &pos}
	if j.O != nil {
		c.old = &View{WithholdPrivate: j.O.W, Hidden: j.O.H.normalized()}
		if j.OS != "" {
			changed, ok := parsePosition(strconv.FormatInt(j.OT, 10), j.OS)
			if !ok {
				return feedCursor{}
			}
			c.changed = &changed
		}
	}
	return c
}

func parsePosition(nanos, sha string) (feedPosition, bool) {
	changedAt, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil || !IsSha256(sha) {
		return feedPosition{}, false
	}
	return feedPosition{changedAt, sha}, true
}
