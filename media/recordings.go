package media

import (
	"encoding/base64"
	"sort"
	"strings"
	"time"
)

// RecordingsPage is a page of the sound recordings view shows, newest first by when each was made (takenAt, else when
// it was uploaded), for an app that lists them (Audio Logs) without reading the whole change feed. With before (an
// older cursor of an earlier page), the page holds the next limit older ones; with after (a newer cursor), the limit
// just newer than it, still newest first; with neither, the newest. older is set when there are older ones than the
// page, newer when there are newer ones; each is the cursor to ask for them. A cursor names a recording's place, not
// the recording, so one deleted since still pages on.
func (s *Store) RecordingsPage(v View, before, after string, limit int) (page []Item, older, newer string) {
	var all []Item
	s.RangeSeen(v, func(item Item) {
		if item.Sidecar.Facts.Kind == KindAudio && !item.Sidecar.Facts.IsJXLOfJPEG() {
			all = append(all, item)
		}
	})
	sort.Slice(all, func(i, j int) bool { return placeOf(all[j]).before(placeOf(all[i])) })

	from, to := 0, len(all) // the page is all[from:to]
	switch {
	case before != "":
		at, ok := decodePlace(before)
		if !ok {
			break
		}
		from = sort.Search(len(all), func(i int) bool { return placeOf(all[i]).before(at) })
		to = min(from+limit, len(all))
	case after != "":
		at, ok := decodePlace(after)
		if !ok {
			break
		}
		to = sort.Search(len(all), func(i int) bool { return !at.before(placeOf(all[i])) })
		from = max(0, to-limit)
	default:
		to = min(limit, len(all))
	}
	if from >= to {
		if from > 0 && from <= len(all) {
			newer = placeOf(all[from-1]).encode()
		}
		return nil, "", newer
	}
	page = all[from:to]
	if to < len(all) {
		older = placeOf(all[to-1]).encode()
	}
	if from > 0 {
		newer = placeOf(all[from]).encode()
	}
	return page, older, newer
}

// place is where a recording stands in RecordingsPage's order: when it was made, then its sha256.
type place struct {
	at  time.Time
	sha string
}

func placeOf(item Item) place {
	at := item.Sidecar.Facts.UploadedAt
	if t := item.Sidecar.Facts.TakenAt; t != nil {
		at = *t
	}
	return place{at.UTC(), item.Sha256()}
}

// before reports whether p comes before o in time (older), the sha256 breaking ties.
func (p place) before(o place) bool {
	if !p.at.Equal(o.at) {
		return p.at.Before(o.at)
	}
	return p.sha < o.sha
}

func (p place) encode() string {
	return base64.RawURLEncoding.EncodeToString([]byte(p.at.Format(time.RFC3339Nano) + "|" + p.sha))
}

func decodePlace(cursor string) (place, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return place{}, false
	}
	at, sha, ok := strings.Cut(string(raw), "|")
	if !ok {
		return place{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return place{}, false
	}
	return place{t.UTC(), sha}, true
}
