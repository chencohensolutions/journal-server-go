package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// uploadPhoto uploads data in one piece from client and returns its sha256.
func (e *testEnv) uploadPhoto(t *testing.T, client *http.Client, data []byte, name string) string {
	t.Helper()
	return e.uploadPhotoWith(t, client, data, name, nil)
}

func TestPrivateFilesAreFlaggedOrWithheldPerDevice(t *testing.T) {
	env := newTestEnv(t)
	phone := env.enrolledClient(t)
	tablet := env.enrolledClient(t)
	devices := env.enrollment.Registry.List("hen")
	if _, err := env.enrollment.Registry.SetWithholdPrivate(devices[1].Serial, true); err != nil {
		t.Fatal(err)
	}

	shown := env.uploadPhoto(t, phone, []byte("a beach"), "BEACH.jpg")
	hidden := env.uploadPhoto(t, phone, []byte("a bedroom"), "BED.jpg")
	feed := func(client *http.Client, since string) (changes []map[string]any, cursor string) {
		t.Helper()
		resp := env.graphql(t, client, `query($s: String) { changes(since: $s) { cursor changes { path deleted media { private } } } }`,
			map[string]any{"s": since})
		var page struct {
			Cursor  string
			Changes []map[string]any
		}
		if err := json.Unmarshal(resp.Data["changes"], &page); err != nil {
			t.Fatalf("changes = %s %+v", resp.Data["changes"], resp.Errors)
		}
		return page.Changes, page.Cursor
	}
	_, tabletCursor := feed(tablet, "")

	marked := env.graphql(t, phone, `mutation($h: [Sha256!]!) { setPrivate(sha256s: $h, private: true) { sha256 private } }`,
		map[string]any{"h": []string{hidden}})
	if got := string(marked.Data["setPrivate"]); got != `[{"sha256":"`+hidden+`","private":true}]` {
		t.Fatalf("setPrivate = %s %+v", got, marked.Errors)
	}

	// The phone gets it flagged, and may fetch it.
	changes, _ := feed(phone, "")
	if len(changes) != 2 || !strings.Contains(mustJSON(changes), `"private":true`) {
		t.Fatalf("phone's feed: %v", changes)
	}
	if resp, _ := phone.Get(env.server.URL + "/media/" + hidden); resp.StatusCode != http.StatusOK {
		t.Fatalf("phone fetching the original: %d", resp.StatusCode)
	}

	// The tablet is told to drop it, and can't reach it in any other way.
	changes, _ = feed(tablet, tabletCursor)
	if len(changes) != 1 || changes[0]["path"] != "media/"+hidden || changes[0]["deleted"] != true || changes[0]["media"] != nil {
		t.Fatalf("tablet's feed: %v", changes)
	}
	if changes, _ := feed(tablet, ""); len(changes) != 1 || changes[0]["path"] != "media/"+shown {
		t.Fatalf("tablet's feed from the start: %v", changes)
	}
	if resp := env.graphql(t, tablet, `query($s: Sha256!) { media(sha256: $s) { sha256 } }`, map[string]any{"s": hidden}); string(resp.Data["media"]) != "null" {
		t.Fatalf("tablet's media = %s", resp.Data["media"])
	}
	for _, path := range []string{"/media/" + hidden, "/media/" + hidden + "/preview?w=100&h=100"} {
		if resp, _ := tablet.Get(env.server.URL + path); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("tablet fetching %s: %d", path, resp.StatusCode)
		}
	}
	unmark := env.graphql(t, tablet, `mutation($h: [Sha256!]!) { setPrivate(sha256s: $h, private: false) { sha256 } }`,
		map[string]any{"h": []string{hidden}})
	if len(unmark.Errors) == 0 {
		t.Fatal("the tablet unmarked a private file")
	}
}

func mustJSON(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}
