package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/faces"
)

func TestTuckedFilesAreFlaggedAndStillSent(t *testing.T) {
	env := newTestEnv(t)
	phone := env.enrolledClient(t)
	tablet := env.enrolledClient(t)
	// Tucking is not private: even a device kept from private files gets a tucked file.
	if _, err := env.enrollment.Registry.SetWithholdPrivate(env.enrollment.Registry.List("hen")[1].Serial, true); err != nil {
		t.Fatal(err)
	}
	info := env.graphql(t, phone, `{ serverInfo { tuckedAway } }`, nil)
	if got := string(info.Data["serverInfo"]); got != `{"tuckedAway":true}` {
		t.Fatalf("serverInfo = %s %+v", got, info.Errors)
	}

	shown := env.uploadPhoto(t, phone, []byte("a beach"), "BEACH.jpg")
	tucked := env.uploadPhoto(t, phone, []byte("an ex"), "EX.jpg")
	feed := func(client *http.Client, since string) (changes []map[string]any, cursor string) {
		t.Helper()
		resp := env.graphql(t, client, `query($s: String) { changes(since: $s) { cursor changes { path deleted media { sha256 tucked private } } } }`,
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

	marked := env.graphql(t, phone, `mutation($h: [Sha256!]!) { setTucked(sha256s: $h, tucked: true) { sha256 tucked private } }`,
		map[string]any{"h": []string{tucked}})
	if got := string(marked.Data["setTucked"]); got != `[{"sha256":"`+tucked+`","tucked":true,"private":false}]` {
		t.Fatalf("setTucked = %s %+v", got, marked.Errors)
	}

	// Both devices get it again, flagged, and may fetch it.
	for name, client := range map[string]*http.Client{"phone": phone, "tablet": tablet} {
		since := ""
		if name == "tablet" {
			since = tabletCursor
		}
		changes, _ := feed(client, since)
		if !strings.Contains(mustJSON(changes), `"sha256":"`+tucked+`","tucked":true`) || strings.Contains(mustJSON(changes), `"deleted":true`) {
			t.Errorf("%s's feed: %v", name, changes)
		}
		if resp, _ := client.Get(env.server.URL + "/media/" + tucked); resp.StatusCode != http.StatusOK {
			t.Errorf("%s fetching the original: %d", name, resp.StatusCode)
		}
	}
	if got := string(env.graphql(t, phone, `query($h: Sha256!) { media(sha256: $h) { tucked } }`, map[string]any{"h": shown}).Data["media"]); got != `{"tucked":false}` {
		t.Errorf("the other photo: %s", got)
	}

	// Brought back from the tablet.
	back := env.graphql(t, tablet, `mutation($h: [Sha256!]!) { setTucked(sha256s: $h, tucked: false) { tucked } }`,
		map[string]any{"h": []string{tucked}})
	if got := string(back.Data["setTucked"]); got != `[{"tucked":false}]` {
		t.Fatalf("bring back = %s %+v", got, back.Errors)
	}

	// Too many at once, or a file the server doesn't have, is refused.
	if resp := env.graphql(t, phone, `mutation { setTucked(sha256s: ["`+strings.Repeat("0", 64)+`"], tucked: true) { sha256 } }`, nil); len(resp.Errors) == 0 {
		t.Error("an unknown file was tucked")
	}
}

// Search never finds a tucked file: not in people, faces or searches, even with includePrivate.
func TestTuckedFilesAreNeverFound(t *testing.T) {
	first := newTestEnv(t)
	uploader := first.enrolledClient(t)
	var mom []string
	for i := range 4 {
		mom = append(mom, first.uploadPhoto(t, uploader, []byte(fmt.Sprintf("mom %d", i)), fmt.Sprintf("MOM%d.jpg", i)))
	}
	first.graphql(t, uploader, `mutation($h: [Sha256!]!) { setTucked(sha256s: $h, tucked: true) { sha256 } }`,
		map[string]any{"h": []string{mom[3]}})
	index, err := faces.OpenIndex(first.dataFolder, fakeFaceModel)
	if err != nil {
		t.Fatal(err)
	}
	for i, sha := range mom {
		index.Add(sha, []faces.Face{personFace(0, i)})
	}
	index.Close()

	env := newTestEnv(t, func(c *auth.AppConfig) {
		c.Users[0].DataFolder = first.dataFolder
		c.FaceModel = fakeFaceModel
	})
	phone := env.enrolledClient(t)
	resp := env.graphql(t, phone, `{ people(includePrivate: true) { people { id files } } }`, nil)
	var got struct {
		People []struct {
			ID    string
			Files int
		}
	}
	if err := json.Unmarshal(resp.Data["people"], &got); err != nil || len(got.People) != 1 || got.People[0].Files != 3 {
		t.Fatalf("people = %s %+v, want one of 3 files", resp.Data["people"], resp.Errors)
	}
	resp = env.graphql(t, phone, `query($p: [ID!]) { searchMedia(people: $p, includePrivate: true) { hits { media { sha256 } } } }`,
		map[string]any{"p": []string{got.People[0].ID}})
	if found := string(resp.Data["searchMedia"]); len(resp.Errors) > 0 || strings.Contains(found, mom[3]) || strings.Count(found, "sha256") != 3 {
		t.Fatalf("searchMedia = %s %+v, want the 3 not tucked", found, resp.Errors)
	}
	resp = env.graphql(t, phone, `query($f: ID!) { similarFaces(face: $f, includePrivate: true) { media { sha256 } } }`,
		map[string]any{"f": mom[0] + ":0"})
	if found := string(resp.Data["similarFaces"]); len(resp.Errors) > 0 || strings.Contains(found, mom[3]) || strings.Count(found, "sha256") != 2 {
		t.Fatalf("similarFaces = %s %+v, want the other 2 not tucked", found, resp.Errors)
	}
}
