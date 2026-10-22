package media

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Changes returns up to limit originals changed after cursor ("" for all), oldest change first, with the cursor
// to continue from. Originals never change, but their sidecars can get a scan's answer later (Sidecar.ChangedAt),
// so a file comes again once scanned; the cursor is opaque to clients, so the feed can later count edits to notes
// and stories too.
//
// view is what the device may see (View): files it may not see still come, for the caller to send as deleted
// (the device may have them from before they were marked private or hidden), except in a feed from the start, which
// has nothing to take back and leaves them out. The cursor remembers the view it was given out for (whether the
// device was kept from private files, and what the user hid); one given out for another view starts the feed
// again, so a device's files are sent or taken back when its setting or the user's hidden collections change.
func (s *Store) Changes(cursor string, limit int, view View) (items []Item, next string, hasMore bool, err error) {
	after, key, err := decodeCursor(cursor)
	if err != nil {
		return nil, "", false, err
	}
	if key != view.key() {
		after = nil
	}
	fresh := cursor == ""
	s.CatchUp()
	s.mu.RLock()
	all := make([]Item, 0, len(s.items))
	for _, item := range s.items {
		if after.before(*item) && !(fresh && s.hides(view, *item)) {
			all = append(all, *item)
		}
	}
	s.mu.RUnlock()
	sort.Slice(all, func(i, j int) bool { return position(all[i]).less(position(all[j])) })
	if len(all) > limit {
		all, hasMore = all[:limit], true
	}
	switch {
	case len(all) > 0:
		next = position(all[len(all)-1]).encode(view.key())
	case after != nil:
		next = after.encode(view.key())
	}
	return all, next, hasMore, nil
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

// A cursor is "m1:<nanos>:<sha>" (or "w1:…" given out while withholding private files) when nothing is hidden, and
// "m2:<view>:<nanos>:<sha>" otherwise, with View.key ("w" or "m", then Hidden.Key).
func (p feedPosition) encode(key string) string {
	var raw []byte
	if len(key) == 1 {
		raw = fmt.Appendf(nil, "%s1:%d:%s", key, p.changedAt, p.sha)
	} else {
		raw = fmt.Appendf(nil, "m2:%s:%d:%s", key, p.changedAt, p.sha)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// ErrBadCursor is a cursor this server didn't hand out.
var ErrBadCursor = errors.New("unknown cursor; start again without one")

// decodeCursor reads a cursor and the view key it was given out for; "" is nil, before everything, for any view.
func decodeCursor(cursor string) (p *feedPosition, key string, err error) {
	if cursor == "" {
		return nil, "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, "", ErrBadCursor
	}
	parts := strings.Split(string(raw), ":")
	switch {
	case len(parts) == 3 && (parts[0] == "m1" || parts[0] == "w1"):
		key = parts[0][:1]
	case len(parts) == 4 && parts[0] == "m2" && len(parts[1]) > 1 && (parts[1][0] == 'm' || parts[1][0] == 'w'):
		key, parts = parts[1], append(parts[:1], parts[2:]...)
	default:
		return nil, "", ErrBadCursor
	}
	var position feedPosition
	position.changedAt, err = strconv.ParseInt(parts[1], 10, 64)
	position.sha = parts[2]
	if err != nil || !IsSha256(position.sha) {
		return nil, "", ErrBadCursor
	}
	return &position, key, nil
}
