package media

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSetNoteKeepsTheAppsObjectAndComesAgainInTheFeed(t *testing.T) {
	f := newFixture(t)
	f.now = f.now.Add(time.Second)
	photo := f.upload([]byte("photo"), "IMG.jpg")
	sha := photo.Sha256()
	_, cursor, _, _ := f.store.Changes("", 10, View{})

	// The first note, with no base. Spacing doesn't matter: it is kept compacted, and its version is of that.
	f.now = f.now.Add(time.Hour)
	item, saved, err := f.store.SetNote(sha, []byte(`{ "v": 1, "title": "שבת",  "future": {"x": 1} }`), "", "Pixel 9")
	if err != nil || !saved {
		t.Fatalf("SetNote = %v %v", saved, err)
	}
	first := item.Sidecar.NoteVersion()
	if got := string(item.Sidecar.NoteContent()); got != `{"v":1,"title":"שבת","future":{"x":1}}` || first == "" {
		t.Fatalf("kept %s (version %q)", got, first)
	}
	items, cursor, _, _ := f.store.Changes(cursor, 10, View{})
	if len(items) != 1 || items[0].Sidecar.NoteVersion() != first {
		t.Fatalf("feed after the note: %+v", items)
	}

	// Another device changed the note meanwhile: a write from the first version is refused and gets the note now kept.
	f.now = f.now.Add(time.Minute)
	item, saved, _ = f.store.SetNote(sha, []byte(`{"v":1,"title":"Shabbat"}`), first, "iPhone")
	second := item.Sidecar.NoteVersion()
	if !saved || second == first {
		t.Fatalf("second write: %v %q", saved, second)
	}
	item, saved, err = f.store.SetNote(sha, []byte(`{"v":1,"title":"late"}`), first, "Pixel 9")
	if err != nil || saved || item.Sidecar.NoteVersion() != second || !strings.Contains(string(item.Sidecar.NoteContent()), "Shabbat") {
		t.Fatalf("a stale base: %v %v %s", saved, err, item.Sidecar.NoteContent())
	}
	// No base on a file that has a note is stale too.
	if _, saved, _ := f.store.SetNote(sha, []byte(`{"v":1}`), "", "Pixel 9"); saved {
		t.Fatal("a note without a base replaced one")
	}

	// The same note again changes nothing and doesn't come again in the feed.
	_, cursor, _, _ = f.store.Changes(cursor, 10, View{})
	f.now = f.now.Add(time.Minute)
	if _, saved, _ := f.store.SetNote(sha, []byte(`{"v":1,"title":"Shabbat"}`), second, "Pixel 9"); !saved {
		t.Fatal("the same note was refused")
	}
	if items, _, _, _ := f.store.Changes(cursor, 10, View{}); len(items) != 0 {
		t.Fatalf("the same note came in the feed: %v", items)
	}

	// Deleted: no content, no version, but a time, so the feed has it and a reopened store reads it.
	f.now = f.now.Add(time.Minute)
	item, saved, err = f.store.SetNote(sha, []byte(`{}`), second, "iPhone")
	if err != nil || !saved || item.Sidecar.NoteVersion() != "" || item.Sidecar.NoteContent() != nil {
		t.Fatalf("delete: %v %v %+v", saved, err, item.Sidecar.Note)
	}
	if items, _, _, _ := f.store.Changes(cursor, 10, View{}); len(items) != 1 {
		t.Fatalf("feed after deleting: %v", items)
	}
	again, err := OpenStore(f.root, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := again.Get(sha)
	if n := got.Sidecar.Note; n == nil || n.SetBy != "iPhone" || !n.SetAt.Equal(f.now) || n.Content != nil {
		t.Fatalf("reopened: %+v", n)
	}

	// Not an object, too large, or an unknown file: refused.
	for _, bad := range []string{`"text"`, `[1]`, `{"v":`} {
		if _, _, err := f.store.SetNote(sha, []byte(bad), "", ""); !errors.Is(err, ErrNoteNotObject) {
			t.Errorf("%s: %v", bad, err)
		}
	}
	huge := `{"text":"` + strings.Repeat("a", MaxNoteSize) + `"}`
	if _, _, err := f.store.SetNote(sha, []byte(huge), "", ""); !errors.Is(err, ErrNoteTooLarge) {
		t.Errorf("huge: %v", err)
	}
	if _, _, err := f.store.SetNote(shaOf([]byte("unknown")), []byte(`{"v":1}`), "", ""); err == nil {
		t.Error("a note for an unknown file was kept")
	}
}

// Marking a file private or tucked, or a scan, keeps its note: each rewrites the sidecar from what is there.
func TestNoteSurvivesOtherMarks(t *testing.T) {
	f := newFixture(t)
	photo := f.upload([]byte("photo"), "IMG.jpg")
	sha := photo.Sha256()
	f.now = f.now.Add(time.Second)
	f.store.SetNote(sha, []byte(`{"v":1,"tags":["Mom"]}`), "", "Pixel 9")
	f.now = f.now.Add(time.Second)
	f.store.SetTucked([]string{sha}, true, "Pixel 9")
	f.store.SetPrivate([]string{sha}, true, "Pixel 9")
	f.store.SetNudity(sha, Detection{Score: 0.1, Model: "test"})
	if got, _ := f.store.Get(sha); string(got.Sidecar.NoteContent()) != `{"v":1,"tags":["Mom"]}` {
		t.Fatalf("note after the marks: %s", got.Sidecar.NoteContent())
	}
}

// A sidecar's note in another shape, or with fields this server doesn't know, is written back as it was.
func TestNoteKeepsWhatItDoesNotKnow(t *testing.T) {
	for _, raw := range []string{`"a line"`, `{"caption":"beach"}`, `{"content":{"v":1},"setAt":"2026-11-08T10:00:00Z","later":[1,2]}`} {
		var sidecar Sidecar
		if err := json.Unmarshal([]byte(`{"format":1,"sha256":"x","facts":{},"note":`+raw+`}`), &sidecar); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		data, err := json.Marshal(sidecar)
		if err != nil {
			t.Fatal(err)
		}
		var back struct{ Note json.RawMessage }
		json.Unmarshal(data, &back)
		var want, got any
		json.Unmarshal([]byte(raw), &want)
		json.Unmarshal(back.Note, &got)
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s came back as %s", raw, back.Note)
		}
	}
}
