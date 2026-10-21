package media

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
)

// Changes returns up to limit originals changed after cursor ("" for all), oldest change first, with the cursor
// to continue from. Originals never change, but their sidecars can get a scan's answer later (Sidecar.ChangedAt),
// so a file comes again once scanned; the cursor is opaque to clients, so the feed can later count edits to notes
// and stories too.
//
// withholdPrivate is for devices kept from private files: their private files still come, for the caller to send
// as deleted (the device may have them from before they were marked), except in a feed from the start, which has
// nothing to take back and leaves them out. The cursor remembers whether it was given out withholding; one from the
// other way starts the feed again, so a device's files are sent or taken back when its setting changes.
func (s *Store) Changes(cursor string, limit int, withholdPrivate bool) (items []Item, next string, hasMore bool, err error) {
	after, withheld, err := decodeCursor(cursor)
	if err != nil {
		return nil, "", false, err
	}
	if withheld != withholdPrivate {
		after = nil
	}
	fresh := cursor == ""
	s.CatchUp()
	s.mu.RLock()
	all := make([]Item, 0, len(s.items))
	for _, item := range s.items {
		if after.before(*item) && !(fresh && withholdPrivate && item.Sidecar.IsPrivate()) {
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
		next = position(all[len(all)-1]).encode(withholdPrivate)
	case after != nil:
		next = after.encode(withholdPrivate)
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

// Cursors given out while withholding private files start with "w1", the others with "m1".
func (p feedPosition) encode(withholdPrivate bool) string {
	prefix := "m1"
	if withholdPrivate {
		prefix = "w1"
	}
	return base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, "%s:%d:%s", prefix, p.changedAt, p.sha))
}

// ErrBadCursor is a cursor this server didn't hand out.
var ErrBadCursor = errors.New("unknown cursor; start again without one")

// decodeCursor reads a cursor; "" is nil, before everything.
func decodeCursor(cursor string) (p *feedPosition, withheld bool, err error) {
	if cursor == "" {
		return nil, false, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, false, ErrBadCursor
	}
	var position feedPosition
	if _, err := fmt.Sscanf(string(raw), "m1:%d:%s", &position.changedAt, &position.sha); err != nil {
		withheld = true
		if _, err := fmt.Sscanf(string(raw), "w1:%d:%s", &position.changedAt, &position.sha); err != nil {
			return nil, false, ErrBadCursor
		}
	}
	if !IsSha256(position.sha) {
		return nil, false, ErrBadCursor
	}
	return &position, withheld, nil
}
