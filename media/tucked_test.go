package media

import (
	"testing"
	"time"
)

func TestSetTuckedTucksTheWholeGroupAndComesAgainInTheFeed(t *testing.T) {
	f := newFixture(t)
	f.now = f.now.Add(time.Second)
	photo := f.upload([]byte("photo"), "IMG.jpg")
	sha := photo.Sha256()
	edit := []byte("edit")
	req := photoRequest(edit, "IMG.jpg")
	req.VersionOf = &sha
	f.start(req)
	f.chunk(edit, 0, int64(len(edit)))
	other := f.upload([]byte("other"), "OTHER.jpg")
	_, cursor, _, _ := f.store.Changes("", 10, View{})

	// Tucking the edit tucks the photo it edits too, and nothing else; it is not private.
	f.now = f.now.Add(time.Hour)
	changed, err := f.store.SetTucked([]string{shaOf(edit)}, true, "Pixel 9")
	if err != nil || len(changed) != 2 {
		t.Fatalf("SetTucked = %v %v", changed, err)
	}
	items, cursor, _, _ := f.store.Changes(cursor, 10, View{})
	if len(items) != 2 || !items[0].Sidecar.IsTucked() || !items[1].Sidecar.IsTucked() || items[0].Sidecar.IsPrivate() {
		t.Fatalf("feed after tucking: %+v", items)
	}
	if got, _ := f.store.Get(other.Sha256()); got.Sidecar.IsTucked() {
		t.Fatal("the other photo was tucked")
	}
	// A tucked file is still shown to a device: tucking is the apps' to hide.
	if got, _ := f.store.Get(sha); f.store.Hides(View{WithholdPrivate: true}, got) {
		t.Fatal("a tucked file is hidden by the server")
	}

	// Tucked again: nothing changes. An edit uploaded later is tucked from the start.
	if changed, _ := f.store.SetTucked([]string{sha}, true, "Pixel 9"); len(changed) != 0 {
		t.Fatalf("tucked twice: %v", changed)
	}
	later := []byte("later edit")
	req = photoRequest(later, "IMG.jpg")
	req.VersionOf = &sha
	f.start(req)
	if result, _ := f.chunk(later, 0, int64(len(later))); !result.Item.Sidecar.IsTucked() {
		t.Fatal("a later edit of a tucked photo isn't tucked")
	}

	// Private and tucked are separate marks: marking one keeps the other.
	f.now = f.now.Add(time.Hour)
	f.store.SetPrivate([]string{sha}, true, "Pixel 9")
	if got, _ := f.store.Get(sha); !got.Sidecar.IsTucked() || !got.Sidecar.IsPrivate() {
		t.Fatalf("both marks: %+v %+v", got.Sidecar.Tucked, got.Sidecar.Privacy)
	}
	_, cursor, _, _ = f.store.Changes(cursor, 10, View{})

	// Brought back: kept in the sidecar with its time, so a reopened store reads it, and the feed has it again.
	f.now = f.now.Add(time.Hour)
	if changed, err := f.store.SetTucked([]string{sha}, false, "iPhone"); err != nil || len(changed) != 3 {
		t.Fatalf("bring back = %v %v", changed, err)
	}
	if items, _, _, _ := f.store.Changes(cursor, 10, View{}); len(items) != 3 {
		t.Fatalf("feed after bringing back: %v", items)
	}
	again, err := OpenStore(f.root, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := again.Get(sha)
	if tuck := got.Sidecar.Tucked; tuck == nil || tuck.Tucked || tuck.SetBy != "iPhone" || !tuck.SetAt.Equal(f.now) || !got.Sidecar.IsPrivate() {
		t.Fatalf("reopened: %+v %+v", tuck, got.Sidecar.Privacy)
	}

	if _, err := f.store.SetTucked([]string{shaOf([]byte("unknown"))}, true, ""); err == nil {
		t.Fatal("an unknown file was accepted")
	}
}
