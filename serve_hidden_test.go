package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestHiddenCollectionsAreHiddenOnEveryDevice(t *testing.T) {
	env := newTestEnv(t)
	phone := env.enrolledClient(t)
	tablet := env.enrolledClient(t)
	photo := env.uploadPhoto(t, phone, []byte("a beach"), "20251101_110819.jpg")
	bumble := env.uploadPhoto(t, phone, []byte("a profile"), "Screenshot_20240301_030715_Bumble.jpg")
	feed := func(client *http.Client, since string) (changes []map[string]any, cursor string) {
		t.Helper()
		resp := env.graphql(t, client, `query($s: String) { changes(since: $s) { cursor changes { path deleted media { source app } } } }`,
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
	changes, tabletCursor := feed(tablet, "")
	if !strings.Contains(mustJSON(changes), `{"app":"Bumble","source":"screenshots"}`) {
		t.Fatalf("before hiding: %v", changes)
	}

	const collections = `{ whatsapp { name files hidden } screenshotsWithoutApp { files hidden } screenshotApps { name files hidden } }`
	set := env.graphql(t, phone, `mutation($i: HiddenCollectionsInput!) { setHiddenCollections(input: $i) `+collections+` }`,
		map[string]any{"i": map[string]any{"whatsapp": true, "screenshotsWithoutApp": false, "screenshotApps": []string{"bumble"}}})
	want := `{"whatsapp":{"name":"WhatsApp","files":0,"hidden":true},"screenshotsWithoutApp":{"files":0,"hidden":false},"screenshotApps":[{"name":"Bumble","files":1,"hidden":true}]}`
	if got := string(set.Data["setHiddenCollections"]); got != want {
		t.Fatalf("setHiddenCollections = %s %+v", got, set.Errors)
	}
	if got := string(env.graphql(t, tablet, `{ hiddenCollections `+collections+` }`, nil).Data["hiddenCollections"]); got != want {
		t.Fatalf("hiddenCollections = %s", got)
	}

	// Every device is told to drop it, and can't reach it in any other way.
	changes, tabletCursor = feed(tablet, tabletCursor)
	if len(changes) != 2 || !strings.Contains(mustJSON(changes), `"deleted":true,"media":null,"path":"media/`+bumble+`"`) {
		t.Fatalf("tablet's feed: %v", changes)
	}
	if changes, _ := feed(phone, ""); len(changes) != 1 || changes[0]["path"] != "media/"+photo {
		t.Fatalf("phone's feed from the start: %v", changes)
	}
	if resp := env.graphql(t, phone, `query($s: Sha256!) { media(sha256: $s) { sha256 } }`, map[string]any{"s": bumble}); string(resp.Data["media"]) != "null" {
		t.Fatalf("media = %s", resp.Data["media"])
	}
	for _, path := range []string{"/media/" + bumble, "/media/" + bumble + "/preview?w=100&h=100"} {
		if resp, _ := phone.Get(env.server.URL + path); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("fetching %s: %d", path, resp.StatusCode)
		}
	}
	if got := string(env.graphql(t, phone, `{ storageUsage { photos } }`, nil).Data["storageUsage"]); got != `{"photos":1}` {
		t.Fatalf("storageUsage = %s", got)
	}
	// Backup still knows the server has it, so it is never sent again.
	if got := string(env.graphql(t, phone, `query($h: [Sha256!]!) { missingMedia(hashes: $h) }`, map[string]any{"h": []string{bumble}}).Data["missingMedia"]); got != `[]` {
		t.Fatalf("missingMedia = %s", got)
	}

	// Shown again: it comes back.
	env.graphql(t, phone, `mutation($i: HiddenCollectionsInput!) { setHiddenCollections(input: $i) { whatsapp { hidden } } }`,
		map[string]any{"i": map[string]any{"whatsapp": false, "screenshotsWithoutApp": false, "screenshotApps": []string{}}})
	changes, _ = feed(tablet, tabletCursor)
	if len(changes) != 2 || strings.Contains(mustJSON(changes), `"deleted":true`) {
		t.Fatalf("shown again: %v", changes)
	}
}
