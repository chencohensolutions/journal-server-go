package search

import (
	"math"
	"testing"
	"time"
)

func TestGroupsSetsAndMoments(t *testing.T) {
	at := func(minute int) *time.Time {
		v := time.Date(2026, 10, 8, 21, 0, 0, 0, time.UTC).Add(time.Duration(minute) * time.Minute)
		return &v
	}
	// Unit vectors at an angle (in degrees) from the first axis: cos(10°) = 0.985, cos(30°) = 0.866.
	angle := func(deg float64) []float32 {
		r := deg * math.Pi / 180
		return []float32{float32(math.Cos(r)), float32(math.Sin(r))}
	}
	type photo struct {
		vector []float32
		at     *time.Time
	}
	photos := map[string]photo{
		"pose":         {angle(0), at(0)},
		"pose again":   {angle(10), at(1)},      // almost identical, a minute later: same set
		"reframed":     {angle(30), at(1)},      // same minute, looks different: own set, same moment
		"later":        {angle(5), at(30)},      // looks the same, but half an hour later: own set, same moment
		"next morning": {angle(0), at(14 * 60)}, // another moment
		"undated":      {angle(0), nil},         // a set and moment of its own
		"pose 3":       {angle(-10), at(2)},     // joins the first set (20° from "pose again", but 10° from its first)
	}
	order := []string{"pose", "reframed", "pose again", "later", "next morning", "undated", "pose 3"}
	hits := make([]Hit, len(order))
	for i, sha := range order {
		hits[i] = Hit{Sha256: sha}
	}
	sets, moments := Groups(hits,
		func(sha string) []float32 { return photos[sha].vector },
		func(sha string) *time.Time { return photos[sha].at })

	wantSets := []int{0, 1, 0, 2, 3, 4, 0}
	wantMoments := []int{0, 0, 0, 0, 1, 2, 0}
	for i, sha := range order {
		if sets[i] != wantSets[i] || moments[i] != wantMoments[i] {
			t.Errorf("%s: set %d moment %d, want set %d moment %d", sha, sets[i], moments[i], wantSets[i], wantMoments[i])
		}
	}
}
