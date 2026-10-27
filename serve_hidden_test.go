package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/media"
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

	const collections = `{ whatsapp { name files hidden } screenshotsWithoutApp { files hidden } screenshotApps { name files imported hidden } }`
	set := env.graphql(t, phone, `mutation($i: HiddenCollectionsInput!) { setHiddenCollections(input: $i) `+collections+` }`,
		map[string]any{"i": map[string]any{"whatsapp": true, "screenshotsWithoutApp": false, "screenshotApps": []string{"bumble"}}})
	want := `{"whatsapp":{"name":"WhatsApp","files":0,"hidden":true},"screenshotsWithoutApp":{"files":0,"hidden":false},"screenshotApps":[{"name":"Bumble","files":1,"imported":0,"hidden":true}]}`
	if got := string(set.Data["setHiddenCollections"]); got != want {
		t.Fatalf("setHiddenCollections = %s %+v", got, set.Errors)
	}
	if got := string(env.graphql(t, tablet, `{ hiddenCollections `+collections+` }`, nil).Data["hiddenCollections"]); got != want {
		t.Fatalf("hiddenCollections = %s", got)
	}

	// Every device is told to drop it (and only it), and can't reach it in any other way.
	changes, tabletCursor = feed(tablet, tabletCursor)
	if len(changes) != 1 || !strings.Contains(mustJSON(changes), `"deleted":true,"media":null,"path":"media/`+bumble+`"`) {
		t.Fatalf("tablet's feed: %v", changes)
	}
	// A feed from the start has it too, as deleted: the phone leaves out its own copy.
	if changes, _ := feed(phone, ""); len(changes) != 2 || changes[0]["path"] != "media/"+photo ||
		!strings.Contains(mustJSON(changes), `"deleted":true,"media":null,"path":"media/`+bumble+`"`) {
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
	if len(changes) != 1 || changes[0]["path"] != "media/"+bumble || strings.Contains(mustJSON(changes), `"deleted":true`) {
		t.Fatalf("shown again: %v", changes)
	}
}

// What the nudity scan scored high is hidden like a hidden collection, from 0.5 until the user chooses: the feed says
// so (hidden), so the phone leaves out its own copy too.
func TestNudityIsHiddenOnEveryDevice(t *testing.T) {
	env := newTestEnv(t)
	phone := env.enrolledClient(t)
	beach := env.uploadPhoto(t, phone, []byte("a beach"), "20251101_110819.jpg")
	nude := env.uploadPhoto(t, phone, []byte("a nude"), "20251101_110820.jpg")
	feed := func(since string) (changes []map[string]any, cursor string) {
		t.Helper()
		resp := env.graphql(t, phone, `query($s: String) { changes(since: $s) { cursor changes { path deleted hidden media { nudity } } } }`,
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
	_, cursor := feed("")

	// The scan runs beside the server, as scripts/scan-nudity does.
	scan, err := media.OpenStore(env.dataFolder, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for sha, score := range map[string]float64{beach: 0.1, nude: 0.8} {
		if err := scan.SetNudity(sha, media.Detection{Score: score, Model: "nudenet-320n"}); err != nil {
			t.Fatal(err)
		}
	}
	changes, cursor := feed(cursor)
	if got := mustJSON(changes); len(changes) != 2 || !strings.Contains(got, `{"deleted":true,"hidden":true,"media":null,"path":"media/`+nude+`"}`) {
		t.Fatalf("after the scan: %s", got)
	}
	const nudity = `{ nudity { hidden from files notScanned } }`
	if got := string(env.graphql(t, phone, `{ hiddenCollections `+nudity+` }`, nil).Data["hiddenCollections"]); got != `{"nudity":{"hidden":true,"from":0.5,"files":1,"notScanned":0}}` {
		t.Fatalf("hiddenCollections = %s", got)
	}
	if resp, _ := phone.Get(env.server.URL + "/media/" + nude); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("fetching it: %d", resp.StatusCode)
	}

	// From 0.9 it is shown again; the feed starts over. Collections left out of the input stay as they were.
	set := func(input map[string]any) string {
		t.Helper()
		input["whatsapp"], input["screenshotsWithoutApp"], input["screenshotApps"] = false, false, []string{}
		resp := env.graphql(t, phone, `mutation($i: HiddenCollectionsInput!) { setHiddenCollections(input: $i) `+nudity+` }`, map[string]any{"i": input})
		if len(resp.Errors) > 0 {
			return resp.Errors[0].Message
		}
		return string(resp.Data["setHiddenCollections"])
	}
	if got := set(map[string]any{"nudity": map[string]any{"hidden": true, "from": 0.9}}); got != `{"nudity":{"hidden":true,"from":0.9,"files":0,"notScanned":0}}` {
		t.Fatalf("from 0.9: %s", got)
	}
	if changes, _ = feed(cursor); strings.Contains(mustJSON(changes), `"deleted":true`) {
		t.Fatalf("shown again: %v", changes)
	}
	if got := set(map[string]any{}); got != `{"nudity":{"hidden":true,"from":0.9,"files":0,"notScanned":0}}` {
		t.Fatalf("left out: %s", got)
	}
	if got := set(map[string]any{"nudity": map[string]any{"hidden": false, "from": 0.9}}); got != `{"nudity":{"hidden":false,"from":0.5,"files":1,"notScanned":0}}` {
		t.Fatalf("off: %s", got)
	}
	if got := set(map[string]any{"nudity": map[string]any{"hidden": true, "from": 0}}); !strings.Contains(got, "more than 0") {
		t.Fatalf("from 0: %s", got)
	}
}

// Sent to you (source received) is hidden and shown again by sentToYou; left out, it stays as it is.
func TestSentToYouIsHiddenOnEveryDevice(t *testing.T) {
	env := newTestEnv(t)
	phone := env.enrolledClient(t)
	if resp := env.graphql(t, phone, `mutation { setCamera(make: "canon", model: "canon eos 80d", mine: false) { make } }`, nil); len(resp.Errors) > 0 {
		t.Fatalf("setCamera: %+v", resp.Errors)
	}
	sent := env.uploadPhotoWith(t, phone, cameraDNG("Canon", "Canon EOS 80D", 1), "IMG_0001.dng", map[string]any{"source": "camera"})
	mine := env.uploadPhotoWith(t, phone, []byte("a beach"), "20251101_110819.jpg", nil)
	media := func(sha string) string {
		t.Helper()
		return string(env.graphql(t, phone, `query($s: Sha256!) { media(sha256: $s) { source } }`, map[string]any{"s": sha}).Data["media"])
	}
	if got := media(sent); got != `{"source":"received"}` {
		t.Fatalf("sent: %s", got)
	}

	const collections = `{ sentToYou { name files imported hidden } }`
	set := func(input map[string]any) string {
		t.Helper()
		input["screenshotsWithoutApp"], input["screenshotApps"] = false, []string{}
		resp := env.graphql(t, phone, `mutation($i: HiddenCollectionsInput!) { setHiddenCollections(input: $i) `+collections+` }`, map[string]any{"i": input})
		if len(resp.Errors) > 0 {
			t.Fatalf("setHiddenCollections: %+v", resp.Errors)
		}
		return string(resp.Data["setHiddenCollections"])
	}
	const hidden, shown = `{"sentToYou":{"name":"Sent to you","files":1,"imported":0,"hidden":true}}`, `{"sentToYou":{"name":"Sent to you","files":1,"imported":0,"hidden":false}}`
	if got := string(env.graphql(t, phone, `{ hiddenCollections `+collections+` }`, nil).Data["hiddenCollections"]); got != shown {
		t.Fatalf("hiddenCollections = %s", got)
	}
	if got := set(map[string]any{"sentToYou": true}); got != hidden {
		t.Fatalf("hiding: %s", got)
	}
	if media(sent) != "null" || media(mine) == "null" {
		t.Fatal("only what was sent is hidden")
	}
	// Left out, and beside apps: it stays hidden.
	if got := set(map[string]any{"apps": []string{"Telegram"}}); got != hidden {
		t.Fatalf("left out: %s", got)
	}
	if got := set(map[string]any{"sentToYou": false}); got != shown {
		t.Fatalf("showing: %s", got)
	}
	if media(sent) == "null" {
		t.Fatal("not shown again")
	}
}
