package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A note written on one device reaches the others through the change feed; a write from an old version is answered
// with the note now kept, and a device kept from private files never gets a private file's note.
func TestNotesSyncThroughTheFeed(t *testing.T) {
	env := newTestEnv(t)
	phone := env.enrolledClient(t)
	tablet := env.enrolledClient(t)
	withheld := env.enrolledClient(t)
	if _, err := env.enrollment.Registry.SetWithholdPrivate(env.enrollment.Registry.List("hen")[2].Serial, true); err != nil {
		t.Fatal(err)
	}
	if got := string(env.graphql(t, phone, `{ serverInfo { notes } }`, nil).Data["serverInfo"]); got != `{"notes":true}` {
		t.Fatalf("serverInfo = %s", got)
	}
	sha := env.uploadPhoto(t, phone, []byte("a beach"), "BEACH.jpg")

	type media struct {
		Sha256      string
		Note        *string
		NoteVersion *string
	}
	setNote := func(client *http.Client, note any, base any) (typename string, m media) {
		t.Helper()
		resp := env.graphql(t, client, `mutation($s: Sha256!, $n: String, $b: String) {
			setNote(sha256: $s, note: $n, baseVersion: $b) {
				__typename
				... on NoteSaved { media { sha256 note noteVersion } }
				... on NoteConflict { media { sha256 note noteVersion } }
			} }`, map[string]any{"s": sha, "n": note, "b": base})
		var got struct {
			Typename string `json:"__typename"`
			Media    media
		}
		if err := json.Unmarshal(resp.Data["setNote"], &got); err != nil {
			t.Fatalf("setNote = %s %+v", resp.Data["setNote"], resp.Errors)
		}
		return got.Typename, got.Media
	}
	feed := func(client *http.Client, since string) (map[string]media, string) {
		t.Helper()
		resp := env.graphql(t, client, `query($s: String) { changes(since: $s) { cursor changes { path deleted media { sha256 note noteVersion } } } }`,
			map[string]any{"s": since})
		var page struct {
			Cursor  string
			Changes []struct {
				Path    string
				Deleted bool
				Media   *media
			}
		}
		if err := json.Unmarshal(resp.Data["changes"], &page); err != nil {
			t.Fatalf("changes = %s %+v", resp.Data["changes"], resp.Errors)
		}
		got := map[string]media{}
		for _, c := range page.Changes {
			if c.Media != nil {
				got[c.Media.Sha256] = *c.Media
			} else {
				got[strings.TrimPrefix(c.Path, "media/")] = media{}
			}
		}
		return got, page.Cursor
	}
	before, tabletCursor := feed(tablet, "")
	_, withheldCursor := feed(withheld, "")
	if m := before[sha]; m.Note != nil || m.NoteVersion != nil {
		t.Fatalf("a note before any was written: %+v", m)
	}

	kind, saved := setNote(phone, `{"v":1,"title":"Beach day","tags":["Sea"]}`, nil)
	if kind != "NoteSaved" || saved.Note == nil || *saved.Note != `{"v":1,"title":"Beach day","tags":["Sea"]}` || saved.NoteVersion == nil {
		t.Fatalf("setNote = %s %+v", kind, saved)
	}
	changes, tabletCursor := feed(tablet, tabletCursor)
	if got := changes[sha]; got.NoteVersion == nil || *got.NoteVersion != *saved.NoteVersion {
		t.Fatalf("the tablet's feed: %+v", changes)
	}

	// The tablet edits from that version; the phone, still on none, is told the note it missed.
	kind, edited := setNote(tablet, `{"v":1,"title":"Beach day!"}`, *saved.NoteVersion)
	if kind != "NoteSaved" {
		t.Fatalf("the tablet's edit: %s", kind)
	}
	kind, conflict := setNote(phone, `{"v":1,"title":"Other"}`, *saved.NoteVersion)
	if kind != "NoteConflict" || *conflict.NoteVersion != *edited.NoteVersion || *conflict.Note != `{"v":1,"title":"Beach day!"}` {
		t.Fatalf("a stale write: %s %+v", kind, conflict)
	}

	// Private: the withheld device gets the file as deleted, note and all, and can't write one.
	env.graphql(t, phone, `mutation($h: [Sha256!]!) { setPrivate(sha256s: $h, private: true) { sha256 } }`, map[string]any{"h": []string{sha}})
	hidden, _ := feed(withheld, withheldCursor)
	if m, ok := hidden[sha]; !ok || m.Note != nil {
		t.Fatalf("the withheld device's feed: %+v", hidden)
	}
	if resp := env.graphql(t, withheld, `mutation($s: Sha256!) { setNote(sha256: $s, note: "{}") { __typename } }`, map[string]any{"s": sha}); len(resp.Errors) == 0 {
		t.Error("a device kept from private files wrote a private file's note")
	}

	// Not an object: refused with a code.
	resp := env.graphql(t, phone, `mutation($s: Sha256!, $b: String) { setNote(sha256: $s, note: "[1]", baseVersion: $b) { __typename } }`,
		map[string]any{"s": sha, "b": *edited.NoteVersion})
	if len(resp.Errors) == 0 || !strings.Contains(mustJSON(resp.Errors), "INVALID_ARGUMENT") {
		t.Errorf("a list as a note: %+v", resp.Errors)
	}

	// Deleted: the feed brings the file with no note.
	if kind, m := setNote(tablet, nil, *edited.NoteVersion); kind != "NoteSaved" || m.Note != nil || m.NoteVersion != nil {
		t.Fatalf("delete = %s %+v", kind, m)
	}
	if changes, _ := feed(tablet, tabletCursor); changes[sha].Sha256 != sha || changes[sha].Note != nil {
		t.Fatalf("feed after deleting: %+v", changes)
	}
}
