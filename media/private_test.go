package media

import (
	"testing"
	"time"
)

func TestSetPrivateMarksTheWholeGroupAndComesAgainInTheFeed(t *testing.T) {
	f := newFixture(t)
	f.now = f.now.Add(time.Second)
	photo := f.upload([]byte("photo"), "IMG.jpg")
	edit := []byte("edit")
	req := photoRequest(edit, "IMG.jpg")
	sha := photo.Sha256()
	req.VersionOf = &sha
	f.start(req)
	f.chunk(edit, 0, int64(len(edit)))
	other := f.upload([]byte("other"), "OTHER.jpg")
	_, cursor, _, _ := f.store.Changes("", 10, View{})

	// Marking the edit marks the photo it edits too; the other photo is left alone.
	f.now = f.now.Add(time.Hour)
	changed, err := f.store.SetPrivate([]string{shaOf(edit)}, true, "Pixel 9")
	if err != nil || len(changed) != 2 {
		t.Fatalf("SetPrivate = %v %v", changed, err)
	}
	items, cursor, _, _ := f.store.Changes(cursor, 10, View{})
	if len(items) != 2 || !items[0].Sidecar.IsPrivate() || !items[1].Sidecar.IsPrivate() {
		t.Fatalf("feed after marking: %+v", items)
	}
	if got, _ := f.store.Get(other.Sha256()); got.Sidecar.IsPrivate() {
		t.Fatal("the other photo was marked")
	}

	// Marked again: nothing changes. An edit uploaded later is private from the start.
	if changed, _ := f.store.SetPrivate([]string{sha}, true, "Pixel 9"); len(changed) != 0 {
		t.Fatalf("marked twice: %v", changed)
	}
	later := []byte("later edit")
	req = photoRequest(later, "IMG.jpg")
	req.VersionOf = &sha
	f.start(req)
	if result, _ := f.chunk(later, 0, int64(len(later))); !result.Item.Sidecar.IsPrivate() {
		t.Fatal("a later edit of a private photo isn't private")
	}

	// Unmarked: kept in the sidecar with its time, so a reopened store reads it, and the feed has it again.
	f.now = f.now.Add(time.Hour)
	if changed, err := f.store.SetPrivate([]string{sha}, false, "iPhone"); err != nil || len(changed) != 3 {
		t.Fatalf("unmark = %v %v", changed, err)
	}
	if items, _, _, _ := f.store.Changes(cursor, 10, View{}); len(items) != 3 {
		t.Fatalf("feed after unmarking: %v", items)
	}
	again, err := OpenStore(f.root, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := again.Get(sha)
	if p := got.Sidecar.Privacy; p == nil || p.Private || p.SetBy != "iPhone" || !p.SetAt.Equal(f.now) {
		t.Fatalf("reopened: %+v", p)
	}

	if _, err := f.store.SetPrivate([]string{shaOf([]byte("unknown"))}, true, ""); err == nil {
		t.Fatal("an unknown file was accepted")
	}
}

func TestFeedWithholdingPrivateFiles(t *testing.T) {
	f := newFixture(t)
	f.now = f.now.Add(time.Second)
	public := f.upload([]byte("public"), "A.jpg")
	private := f.upload([]byte("private"), "B.jpg")
	f.store.SetPrivate([]string{private.Sha256()}, true, "")

	// A feed from the start leaves private files out: the device has nothing to take back.
	items, withheld, _, _ := f.store.Changes("", 10, View{WithholdPrivate: true})
	if len(items) != 1 || items[0].Sha256() != public.Sha256() {
		t.Fatalf("withholding from the start: %v", items)
	}
	// Marked later: it comes again, for the caller to send as deleted.
	f.now = f.now.Add(time.Hour)
	f.store.SetPrivate([]string{public.Sha256()}, true, "")
	if items, _, _, _ := f.store.Changes(withheld, 10, View{WithholdPrivate: true}); len(items) != 1 || !items[0].Sidecar.IsPrivate() {
		t.Fatalf("marked later: %v", items)
	}

	// A device's setting changes: its cursor from the other way starts the feed again, with everything.
	_, sent, _, _ := f.store.Changes("", 10, View{})
	if items, _, _, _ := f.store.Changes(sent, 10, View{WithholdPrivate: true}); len(items) != 2 {
		t.Fatalf("withholding turned on: %v", items)
	}
	if items, _, _, _ := f.store.Changes(withheld, 10, View{}); len(items) != 2 {
		t.Fatalf("withholding turned off: %v", items)
	}
}
