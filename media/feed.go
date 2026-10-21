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
func (s *Store) Changes(cursor string, limit int) (items []Item, next string, hasMore bool, err error) {
	after, err := decodeCursor(cursor)
	if err != nil {
		return nil, "", false, err
	}
	s.CatchUp()
	s.mu.RLock()
	all := make([]Item, 0, len(s.items))
	for _, item := range s.items {
		if after.before(*item) {
			all = append(all, *item)
		}
	}
	s.mu.RUnlock()
	sort.Slice(all, func(i, j int) bool { return position(all[i]).less(position(all[j])) })
	if len(all) > limit {
		all, hasMore = all[:limit], true
	}
	next = cursor
	if len(all) > 0 {
		next = position(all[len(all)-1]).encode()
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

func (p feedPosition) encode() string {
	return base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, "m1:%d:%s", p.changedAt, p.sha))
}

// ErrBadCursor is a cursor this server didn't hand out.
var ErrBadCursor = errors.New("unknown cursor; start again without one")

func decodeCursor(cursor string) (*feedPosition, error) {
	if cursor == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, ErrBadCursor
	}
	var p feedPosition
	if _, err := fmt.Sscanf(string(raw), "m1:%d:%s", &p.changedAt, &p.sha); err != nil || !IsSha256(p.sha) {
		return nil, ErrBadCursor
	}
	return &p, nil
}
