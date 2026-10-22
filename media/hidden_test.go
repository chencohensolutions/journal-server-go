package media

import (
	"testing"
	"time"
)

func TestHiddenHides(t *testing.T) {
	facts := func(source string, app *string) Facts { return Facts{Source: &source, App: app} }
	bumble, tinder := "Bumble", "Tinder"
	h := Hidden{Sources: []string{SourceWhatsApp}, ScreenshotApps: []string{"bumble"}}
	for _, c := range []struct {
		facts Facts
		want  bool
	}{
		{facts(SourceWhatsApp, nil), true},
		{facts(SourceScreenshots, &bumble), true},
		{facts(SourceScreenshots, &tinder), false},
		{facts(SourceScreenshots, nil), false},
		{facts(SourceCamera, &bumble), false},
		{Facts{}, false},
	} {
		if got := h.Hides(c.facts); got != c.want {
			t.Errorf("%v %v: %v, want %v", *c.facts.Source, c.facts.App, got, c.want)
		}
	}
	h.ScreenshotsWithoutApp = true
	if !h.Hides(facts(SourceScreenshots, nil)) {
		t.Error("a screenshot without an app")
	}
	if (Hidden{}).Key() != "" || h.Key() == "" || h.Key() != (Hidden{Sources: []string{"whatsapp", "whatsapp"}, ScreenshotApps: []string{" BUMBLE "}, ScreenshotsWithoutApp: true}).Key() {
		t.Error("keys")
	}
}

func TestHiddenCollectionsInTheFeed(t *testing.T) {
	f := newFixture(t)
	f.now = f.now.Add(time.Second)
	photo := f.upload([]byte("photo"), "20251101_110819.heic")
	bumble := f.upload([]byte("bumble"), "Screenshot_20240301_030715_Bumble.jpg")
	edit := photoRequest([]byte("bumble edit"), "IMG_EDIT.jpg")
	edit.VersionOf = ptr(bumble.Sha256())
	f.start(edit)
	if result, err := f.chunk([]byte("bumble edit"), 0, 11); err != nil || result.Item == nil {
		t.Fatal(result, err)
	}
	_, cursor, _, _ := f.store.Changes("", 10, f.store.ViewFor(false))

	f.now = f.now.Add(time.Hour)
	if err := f.store.SetHidden(Hidden{ScreenshotApps: []string{"BUMBLE", "bumble", "OkCupid"}}); err != nil {
		t.Fatal(err)
	}
	view := f.store.ViewFor(false)

	// A device that had them gets everything again, to send the hidden ones as deleted (the edit with its photo).
	items, _, _, _ := f.store.Changes(cursor, 10, view)
	hidden := 0
	for _, item := range items {
		if f.store.Hides(view, item) {
			hidden++
		}
	}
	if len(items) != 3 || hidden != 2 {
		t.Fatalf("after hiding: %d items, %d hidden", len(items), hidden)
	}
	// A feed from the start leaves them out; counts too.
	items, shownCursor, _, _ := f.store.Changes("", 10, view)
	if len(items) != 1 || items[0].Sha256() != photo.Sha256() {
		t.Fatalf("from the start: %v", items)
	}
	if u := f.store.Usage(view); u.Photos != 1 {
		t.Fatalf("usage: %+v", u)
	}
	if u := f.store.Usage(View{}); u.Photos != 3 {
		t.Fatalf("usage of everything: %+v", u)
	}

	whatsapp, withoutApp, apps := f.store.Collections()
	if whatsapp.Files != 0 || whatsapp.Hidden || withoutApp.Files != 0 || len(apps) != 2 ||
		apps[0] != (Collection{"Bumble", 1, true}) || apps[1] != (Collection{"OkCupid", 0, true}) {
		t.Fatalf("collections: %+v %+v %+v", whatsapp, withoutApp, apps)
	}

	// Kept in the data folder, as chosen (one spelling per app).
	again, err := OpenStore(f.store.Root(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if h := again.Hidden(); len(h.ScreenshotApps) != 2 || h.ScreenshotApps[0] != "BUMBLE" || h.ScreenshotApps[1] != "OkCupid" {
		t.Fatalf("reopened: %+v", h)
	}

	// Shown again: the feed starts over, and they come back.
	if err := f.store.SetHidden(Hidden{}); err != nil {
		t.Fatal(err)
	}
	view = f.store.ViewFor(false)
	items, _, _, _ = f.store.Changes(shownCursor, 10, view)
	if len(items) != 3 || f.store.Hides(view, items[0]) || f.store.Hides(view, items[1]) || f.store.Hides(view, items[2]) {
		t.Fatalf("shown again: %v", items)
	}
}
