package resolver

import (
	"testing"
	"time"

	"github.com/journal/server-go/graph/model"
	"github.com/journal/server-go/media"
)

func TestSearchFilterKeepsTheYearsAndPlace(t *testing.T) {
	item := func(taken string, at *media.GeoPoint) media.Item {
		var sidecar media.Sidecar
		sidecar.Facts.UploadedAt = time.Date(2026, 10, 19, 22, 0, 0, 0, time.UTC)
		if taken != "" {
			when, err := time.Parse(time.RFC3339, taken)
			if err != nil {
				t.Fatal(err)
			}
			sidecar.Facts.TakenAt = &when
		}
		sidecar.Facts.Location = at
		return media.Item{Sidecar: sidecar}
	}
	paris := &media.GeoPoint{Lat: 48.86, Lon: 2.35}
	fiji := &media.GeoPoint{Lat: -17.7, Lon: 179.9}
	// New Year's night in Tel Aviv: 2025 in UTC, 2026 where it was taken.
	newYear := item("2026-01-01T00:30:00+02:00", paris)

	tests := []struct {
		name  string
		years []int
		area  *model.GeoAreaInput
		item  media.Item
		want  bool
	}{
		{"no filter", nil, nil, item("", nil), true},
		{"the year where it was taken", []int{2026}, nil, newYear, true},
		{"not the year in UTC", []int{2025}, nil, newYear, false},
		{"one of several years", []int{2019, 2026}, nil, newYear, true},
		{"the upload's year when the time isn't known", []int{2026}, nil, item("", nil), true},
		{"inside the area", nil, &model.GeoAreaInput{South: 48.8, West: 2.2, North: 48.9, East: 2.5}, newYear, true},
		{"outside the area", nil, &model.GeoAreaInput{South: 45.7, West: 4.8, North: 45.8, East: 4.9}, newYear, false},
		{"no location", nil, &model.GeoAreaInput{South: -90, West: -180, North: 90, East: 180}, item("", nil), false},
		{"across the 180° meridian", nil, &model.GeoAreaInput{South: -20, West: 179, North: -15, East: -179}, item("", fiji), true},
		{"year and area together", []int{2025}, &model.GeoAreaInput{South: 48.8, West: 2.2, North: 48.9, East: 2.5}, newYear, false},
	}
	for _, tt := range tests {
		f, err := newSearchFilter(tt.years, tt.area)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if got := f.keeps(tt.item); got != tt.want {
			t.Errorf("%s: keeps = %v, want %v", tt.name, got, tt.want)
		}
	}
	if _, err := newSearchFilter(nil, &model.GeoAreaInput{South: 10, North: 5, West: 0, East: 1}); err == nil {
		t.Error("an area with south above north was accepted")
	}
}
