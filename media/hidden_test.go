package media

import (
	"encoding/base64"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestHiddenHides(t *testing.T) {
	facts := func(source string, app *string) Facts { return Facts{Source: &source, ScreenshotApp: app} }
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
			t.Errorf("%v %v: %v, want %v", *c.facts.Source, c.facts.ScreenshotApp, got, c.want)
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

	// A device that had them gets only the hidden ones again, to send as deleted (the edit with its photo).
	items, hiddenCursor, _, _ := f.store.Changes(cursor, 10, view)
	hidden := 0
	for _, item := range items {
		if f.store.Hides(view, item) {
			hidden++
		}
	}
	if len(items) != 2 || hidden != 2 {
		t.Fatalf("after hiding: %d items, %d hidden", len(items), hidden)
	}
	// And then goes on where it was.
	if items, _, _, _ := f.store.Changes(hiddenCursor, 10, view); len(items) != 0 {
		t.Fatalf("after the change-over: %v", items)
	}
	// A feed from the start has them too, for the device's own copies; counts leave them out.
	items, shownCursor, _, _ := f.store.Changes("", 10, view)
	if len(items) != 3 || countShown(f.store, view, items) != 1 || f.store.Hides(view, *f.store.mustGet(t, photo.Sha256())) {
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
		apps[0] != (Collection{"Bumble", 1, 0, true}) || apps[1] != (Collection{"OkCupid", 0, 0, true}) {
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

	// Shown again: they come back, and only they.
	if err := f.store.SetHidden(Hidden{}); err != nil {
		t.Fatal(err)
	}
	view = f.store.ViewFor(false)
	items, _, _, _ = f.store.Changes(shownCursor, 10, view)
	if len(items) != 2 || items[0].Sha256() != bumble.Sha256() || f.store.Hides(view, items[1]) {
		t.Fatalf("shown again: %v", items)
	}
}

// Each collection says how many of its files the import placed, so apps can tell them from what phones uploaded.
func TestCollectionsCountWhatWasImported(t *testing.T) {
	f := newFixture(t)
	place := func(data []byte, name, by string) {
		req := photoRequest(data, name)
		req.UploadedBy = by
		if IsWhatsAppName(name) {
			req.Source = ptr(SourceWhatsApp)
		}
		f.start(req)
		if result, err := f.chunk(data, 0, int64(len(data))); err != nil || result.Item == nil {
			t.Fatal(result, err)
		}
	}
	place([]byte("phone whatsapp"), "IMG-20240301-WA0001.jpg", "Galaxy S25")
	place([]byte("imported whatsapp"), "IMG-20240302-WA0002.jpg", "import")
	place([]byte("imported whatsapp 2"), "IMG-20240303-WA0003.jpg", "import")
	place([]byte("phone bumble"), "Screenshot_20240301_030715_Bumble.jpg", "Galaxy S25")
	place([]byte("imported bumble"), "Screenshot_20240302_101010_Bumble.jpg", "import")
	place([]byte("imported screenshot"), "Screenshot_20240303_101010.jpg", "import")

	whatsapp, withoutApp, apps := f.store.Collections()
	if whatsapp != (Collection{"WhatsApp", 3, 2, false}) || withoutApp != (Collection{"", 1, 1, false}) ||
		len(apps) != 1 || apps[0] != (Collection{"Bumble", 2, 1, false}) {
		t.Fatalf("collections: %+v %+v %+v", whatsapp, withoutApp, apps)
	}
}

// Any app's folder kind can be hidden on every device, compared ignoring case; a settings.json from before WhatsApp
// was a folder kind hides it as folder:WhatsApp.
func TestHidingApps(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(f.store.settingsPath(), []byte(`{"format":1,"hidden":{"sources":["whatsapp"],"nudityFrom":0,"audio":{}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	f.reopen()
	if h := f.store.Hidden(); len(h.Sources) != 1 || h.Sources[0] != SourceWhatsApp {
		t.Fatalf("read: %+v", h)
	}
	place := func(data []byte, name, source string) Item {
		req := photoRequest(data, name)
		req.Source = &source
		f.start(req)
		result, err := f.chunk(data, 0, int64(len(data)))
		if err != nil || result.Item == nil {
			t.Fatal(result, err)
		}
		return *result.Item
	}
	wa := place([]byte("wa"), "x.jpg", OldSourceWhatsApp) // an old app's upload
	tg := place([]byte("tg"), "y.jpg", "folder:telegram")
	pet := place([]byte("pet"), "z.jpg", "folder:Petcube")
	pet2 := place([]byte("pet2"), "w.jpg", "folder:PETCUBE")
	if *wa.Sidecar.Facts.Source != SourceWhatsApp || *tg.Sidecar.Facts.Source != "folder:Telegram" {
		t.Fatalf("sources: %s %s", *wa.Sidecar.Facts.Source, *tg.Sidecar.Facts.Source)
	}
	view := f.store.ViewFor(false)
	if !f.store.Hides(view, wa) || f.store.Hides(view, tg) {
		t.Fatal("WhatsApp hidden from the old settings")
	}
	if err := f.store.SetHidden(Hidden{Sources: []string{"folder:TELEGRAM", "folder:petcube"}}); err != nil {
		t.Fatal(err)
	}
	view = f.store.ViewFor(false)
	if f.store.Hides(view, wa) || !f.store.Hides(view, tg) || !f.store.Hides(view, pet) || !f.store.Hides(view, pet2) {
		t.Fatal("hidden ignoring case")
	}
	apps := f.store.AppCollections()
	if len(apps) != 3 || apps[0].Files != 2 || !apps[0].Hidden || apps[1].Name != "Telegram" || !apps[1].Hidden ||
		apps[2] != (Collection{"WhatsApp", 1, 0, false}) {
		t.Fatalf("apps: %+v", apps)
	}
}

// A change-over is read in pages, then the feed goes on where it was; a view that changes again meanwhile, or a
// cursor from before views were carried, brings everything again.
func TestViewChangeOverIsPagedAndGoesOn(t *testing.T) {
	f := newFixture(t)
	var shots []Item
	for i := range 3 {
		f.now = f.now.Add(time.Second)
		shots = append(shots, f.upload([]byte(fmt.Sprint("shot", i)), fmt.Sprintf("Screenshot_2024030%d_030715_Bumble.jpg", i+1)))
	}
	f.now = f.now.Add(time.Second)
	photo := f.upload([]byte("photo"), "20251101_110819.heic")
	_, cursor, _, _ := f.store.Changes("", 10, f.store.ViewFor(false))

	f.now = f.now.Add(time.Hour)
	if err := f.store.SetHidden(Hidden{ScreenshotApps: []string{"Bumble"}}); err != nil {
		t.Fatal(err)
	}
	view := f.store.ViewFor(false)
	items, next, remaining, _ := f.store.Changes(cursor, 2, view)
	if len(items) != 2 || remaining != 1 || items[0].Sha256() != shots[0].Sha256() || items[1].Sha256() != shots[1].Sha256() {
		t.Fatalf("first page of the change-over: %v, %d remaining", items, remaining)
	}
	// Uploaded during the change-over: comes after it, in the feed's own order.
	f.now = f.now.Add(time.Second)
	later := f.upload([]byte("later"), "20251102_110819.heic")
	items, next, remaining, _ = f.store.Changes(next, 2, view)
	if len(items) != 2 || remaining != 0 || items[0].Sha256() != shots[2].Sha256() || items[1].Sha256() != later.Sha256() {
		t.Fatalf("second page: %v, %d remaining", items, remaining)
	}
	if items, _, _, _ := f.store.Changes(next, 2, view); len(items) != 0 {
		t.Fatalf("after the change-over: %v", items)
	}

	// The view changes again while a change-over is under way: everything again, since what the device has is unknown.
	_, cursor, _, _ = f.store.Changes("", 10, f.store.ViewFor(false))
	if err := f.store.SetHidden(Hidden{}); err != nil {
		t.Fatal(err)
	}
	_, midway, _, _ := f.store.Changes(cursor, 1, f.store.ViewFor(false))
	if err := f.store.SetHidden(Hidden{ScreenshotApps: []string{"Bumble"}, ScreenshotsWithoutApp: true}); err != nil {
		t.Fatal(err)
	}
	if items, _, _, _ := f.store.Changes(midway, 10, f.store.ViewFor(false)); len(items) != 5 {
		t.Fatalf("view changed twice: %d items", len(items))
	}

	// A cursor from before views were carried in it still works, and starts the feed again when the view differs.
	old := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, "m1:%d:%s", position(photo).changedAt, photo.Sha256()))
	if items, _, _, _ := f.store.Changes(old, 10, View{}); len(items) != 1 || items[0].Sha256() != later.Sha256() {
		t.Fatalf("old cursor: %v", items)
	}
	if items, _, _, _ := f.store.Changes(old, 10, f.store.ViewFor(false)); len(items) != 5 {
		t.Fatalf("old cursor for another view: %d items", len(items))
	}
}

// A feed read page by page while something is hidden reaches its end: the cursor keeps the view it was given for.
func TestHiddenFeedPages(t *testing.T) {
	f := newFixture(t)
	for i := range 3 {
		f.now = f.now.Add(time.Second)
		f.upload([]byte(fmt.Sprint("photo", i)), fmt.Sprintf("2025110%d_110819.heic", i+1))
	}
	if err := f.store.SetHidden(Hidden{ScreenshotApps: []string{"Bumble"}}); err != nil {
		t.Fatal(err)
	}
	cursor, seen := "", 0
	for range 5 {
		items, next, remaining, err := f.store.Changes(cursor, 1, f.store.ViewFor(false))
		if err != nil {
			t.Fatal(err)
		}
		seen += len(items)
		if remaining == 0 {
			if seen != 3 {
				t.Errorf("read %d files, want 3", seen)
			}
			return
		}
		cursor = next
	}
	t.Fatal("the feed started again on every page")
}

// Nudity is hidden from 0.5 in a data folder whose settings.json doesn't say (or that has none), and off stays off.
func TestHiddenNudity(t *testing.T) {
	f := newFixture(t)
	f.now = f.now.Add(time.Second)
	nude := f.upload([]byte("nude"), "20251101_110819.heic")
	edit := photoRequest([]byte("nude edit"), "IMG_EDIT.jpg")
	edit.VersionOf = ptr(nude.Sha256())
	f.start(edit)
	if result, err := f.chunk([]byte("nude edit"), 0, 9); err != nil || result.Item == nil {
		t.Fatal(result, err)
	}
	unscanned := f.upload([]byte("beach"), "20251101_110820.heic")
	if err := f.store.SetNudity(nude.Sha256(), Detection{Score: 0.5, Model: "nudenet-320n"}); err != nil {
		t.Fatal(err)
	}
	if f.store.Hidden().NudityFrom != DefaultNudityFrom {
		t.Fatalf("without settings.json: %+v", f.store.Hidden())
	}
	view := f.store.ViewFor(false)
	items, _, _, _ := f.store.Changes("", 10, view)
	if len(items) != 3 || countShown(f.store, view, items) != 1 || f.store.Hides(view, *f.store.mustGet(t, unscanned.Sha256())) {
		t.Fatalf("hidden with its edit: %v", items)
	}
	if files, notScanned := f.store.NudityCounts(0.5); files != 1 || notScanned != 2 {
		t.Fatalf("counts: %d, %d not scanned", files, notScanned)
	}

	// A settings.json written before nudity was hidden here hides it from the default too.
	if err := os.WriteFile(f.store.settingsPath(), []byte(`{"format": 1, "hidden": {"sources": ["whatsapp"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	f.reopen()
	if h := f.store.Hidden(); h.NudityFrom != DefaultNudityFrom || len(h.Sources) != 1 {
		t.Fatalf("older settings.json: %+v", h)
	}
	if err := f.store.SetHidden(Hidden{}); err != nil {
		t.Fatal(err)
	}
	f.reopen()
	if h := f.store.Hidden(); !h.IsEmpty() {
		t.Fatalf("off, reopened: %+v", h)
	}
}

func countShown(store *Store, view View, items []Item) (shown int) {
	for _, item := range items {
		if !store.Hides(view, item) {
			shown++
		}
	}
	return shown
}

func (s *Store) mustGet(t *testing.T, sha string) *Item {
	t.Helper()
	item, ok := s.Get(sha)
	if !ok {
		t.Fatalf("no %s", sha)
	}
	return &item
}
