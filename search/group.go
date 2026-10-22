package search

import "time"

// Grouping search hits the way the apps show them: almost identical photos (a burst, the same shot taken again) as one
// set you swipe, and sets taken close together kept together. Measured on the owner's photos (Oct 22, 2026): frames
// of a burst or a pose taken again score 0.93 to 0.99 against each other; a reframed shot of the same scene seconds
// later, 0.83 to 0.87; the same table half an hour later still 0.95, so time matters as much as looks.
const (
	// SetSimilarity is how close a photo's vector must be to a set's first (best) photo to join it…
	SetSimilarity = 0.93
	// …and SetWindow how close in time.
	SetWindow = 10 * time.Minute
	// MomentWindow keeps sets taken this close to a moment's first set together.
	MomentWindow = 2 * time.Hour
)

// Groups numbers hits (best first) by set and by moment. A hit joins the first set, in the order the sets were
// started, whose first hit was taken within SetWindow of it and whose vector is at least SetSimilarity like its own;
// else it starts a set. A set joins the first moment whose first set was taken within MomentWindow of it; else it
// starts one. Sets and moments are numbered from 0 in the order they start, so by their best hit. vector and takenAt
// give a hit's vector (nil: none) and when it was taken (nil: unknown); a hit without either is a set and moment of
// its own.
func Groups(hits []Hit, vector func(sha string) []float32, takenAt func(sha string) *time.Time) (sets, moments []int) {
	type group struct {
		at     time.Time
		vector []float32
		moment int
	}
	var setGroups []group
	var momentStarts []time.Time
	sets = make([]int, len(hits))
	moments = make([]int, len(hits))
	for i, h := range hits {
		at := takenAt(h.Sha256)
		var v []float32
		if at != nil {
			v = vector(h.Sha256)
		}
		set := -1
		if v != nil {
			for s, g := range setGroups {
				if g.vector != nil && within(*at, g.at, SetWindow) && dot(v, g.vector) >= SetSimilarity {
					set = s
					break
				}
			}
		}
		if set < 0 {
			moment := -1
			if at != nil {
				for m, start := range momentStarts {
					if !start.IsZero() && within(*at, start, MomentWindow) {
						moment = m
						break
					}
				}
			}
			if moment < 0 {
				moment = len(momentStarts)
				var start time.Time
				if at != nil {
					start = *at
				}
				momentStarts = append(momentStarts, start)
			}
			set = len(setGroups)
			g := group{vector: v, moment: moment}
			if at != nil {
				g.at = *at
			}
			setGroups = append(setGroups, g)
		}
		sets[i] = set
		moments[i] = setGroups[set].moment
	}
	return sets, moments
}

func within(a, b time.Time, d time.Duration) bool {
	gap := a.Sub(b)
	return gap <= d && gap >= -d
}

func dot(a, b []float32) float32 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}
