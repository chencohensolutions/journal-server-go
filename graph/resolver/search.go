package resolver

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/journal/server-go/graph/model"
	"github.com/journal/server-go/media"
	"github.com/journal/server-go/search"
)

// maxSearchModelWait is the longest searchModel(wait: true) waits for the model to load (about 4 s, longer on a
// cold disk); well inside /query's 2 minutes to answer.
const maxSearchModelWait = 30 * time.Second

// searchModelState is the schema's name for a search model's state.
func searchModelState(state search.ModelState) model.SearchModelState {
	switch state {
	case search.Ready:
		return model.SearchModelStateReady
	case search.Loading:
		return model.SearchModelStateLoading
	}
	return model.SearchModelStateUnloaded
}

// page is a search's limit (default 100) and offset (default 0), checked.
func page(limit, offset *int) (n, skip int, err error) {
	n = 100
	if limit != nil {
		n = *limit
	}
	if n < 1 || n > maxSearchHits {
		return 0, 0, coded(CodeInvalidArgument, "limit must be 1–%d", maxSearchHits)
	}
	if offset != nil {
		skip = *offset
	}
	if skip < 0 || skip+n > maxSearchDepth {
		return 0, 0, coded(CodeInvalidArgument, "offset must be 0 or more, and offset + limit at most %d", maxSearchDepth)
	}
	return n, skip, nil
}

// lastTaken is the limit files of files that keep accepts, the last taken first (uploaded, when not known): a
// search by people alone.
func lastTaken(v view, files map[string]bool, limit int, keep func(sha string) bool) []search.Hit {
	var items []media.Item
	for sha := range files {
		if item, ok := v.store.Get(sha); ok && keep(sha) {
			items = append(items, item)
		}
	}
	at := func(item media.Item) int64 {
		if t := item.Sidecar.Facts.TakenAt; t != nil {
			return t.UnixNano()
		}
		return item.Sidecar.Facts.UploadedAt.UnixNano()
	}
	slices.SortFunc(items, func(a, b media.Item) int {
		return cmp.Or(cmp.Compare(at(b), at(a)), strings.Compare(a.Sha256(), b.Sha256()))
	})
	hits := make([]search.Hit, 0, min(limit, len(items)))
	for _, item := range items[:min(limit, len(items))] {
		hits = append(hits, search.Hit{Sha256: item.Sha256()})
	}
	return hits
}

// searchFilter is searchMedia's years and area: which files a search may return. Zero value: all.
type searchFilter struct {
	years map[int]bool
	area  *model.GeoAreaInput
}

func newSearchFilter(years []int, area *model.GeoAreaInput) (searchFilter, error) {
	f := searchFilter{area: area}
	if len(years) > 500 {
		return f, coded(CodeInvalidArgument, "years: at most 500")
	}
	if len(years) > 0 {
		f.years = map[int]bool{}
		for _, y := range years {
			f.years[y] = true
		}
	}
	if a := area; a != nil {
		for _, v := range []float64{a.South, a.North, a.West, a.East} {
			if math.IsNaN(v) {
				return f, coded(CodeInvalidArgument, "area: not a number")
			}
		}
		if a.South < -90 || a.North > 90 || a.South > a.North || a.West < -180 || a.West > 180 || a.East < -180 || a.East > 180 {
			return f, coded(CodeInvalidArgument, "area: south ≤ north within ±90, west and east within ±180")
		}
	}
	return f, nil
}

// keeps reports whether item passes the filter. Its year is where it was taken (the offset its time was sent with),
// or the year it was uploaded when that isn't known, as the apps date it.
func (f searchFilter) keeps(item media.Item) bool {
	facts := item.Sidecar.Facts
	if f.years != nil {
		year := facts.UploadedAt.Year()
		if facts.TakenAt != nil {
			year = facts.TakenAt.Year()
		}
		if !f.years[year] {
			return false
		}
	}
	if a := f.area; a != nil {
		at := facts.Location
		if at == nil || at.Lat < a.South || at.Lat > a.North {
			return false
		}
		if a.West <= a.East {
			return at.Lon >= a.West && at.Lon <= a.East
		}
		// Across the 180° meridian.
		return at.Lon >= a.West || at.Lon <= a.East
	}
	return true
}
