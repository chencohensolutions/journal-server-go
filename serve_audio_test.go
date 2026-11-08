package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/media"
)

// TestSoundRecordingsAreHiddenUntilShown imports a call as journal-server import does: devices don't get it until the user
// shows calls, then it plays from /media with Range, has no preview, and its contact can be corrected.
func TestSoundRecordingsAreHiddenUntilShown(t *testing.T) {
	env := newTestEnv(t)
	phone := env.enrolledClient(t)
	store := env.library.Store("hen")
	data := []byte("pretend this is the sound of a phone call")
	path := filepath.Join(t.TempDir(), "Call recording 0501234567_250726_152307.m4a")
	os.WriteFile(path, data, 0o644)
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	started := time.Date(2025, 7, 26, 15, 23, 7, 0, time.FixedZone("", 3*3600))
	ms, number, source := int64(90_000), "0501234567", media.SourceOther
	if _, _, err := store.Import(path, media.UploadRequest{
		Sha256: sha, Size: int64(len(data)), FileName: filepath.Base(path), MimeType: "audio/mp4", Kind: media.KindAudio,
		TakenAt: &started, DurationMs: &ms, Source: &source, UploadedBy: "import", Call: &media.Call{Number: &number},
	}); err != nil {
		t.Fatal(err)
	}

	const collections = `{ recordings { files hidden } calls { files hidden } voiceMessages { files hidden } }`
	if got := string(env.graphql(t, phone, `{ hiddenCollections `+collections+` }`, nil).Data["hiddenCollections"]); got !=
		`{"recordings":{"files":0,"hidden":true},"calls":{"files":1,"hidden":true},"voiceMessages":{"files":0,"hidden":true}}` {
		t.Fatalf("hiddenCollections = %s", got)
	}
	feed := `{ changes { changes { deleted hidden media { kind takenAt endedAt durationMs call { contact number } } } } }`
	if got := string(env.graphql(t, phone, feed, nil).Data["changes"]); got != `{"changes":[{"deleted":true,"hidden":true,"media":null}]}` {
		t.Fatalf("hidden: changes = %s", got)
	}
	if resp, _ := phone.Get(env.server.URL + "/media/" + sha); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("hidden: /media answered %d", resp.StatusCode)
	}

	// Shown; the rest of what is hidden stays as it is.
	set := env.graphql(t, phone, `mutation($i: HiddenCollectionsInput!) { setHiddenCollections(input: $i) `+collections+` }`,
		map[string]any{"i": map[string]any{"whatsapp": false, "screenshotsWithoutApp": false, "screenshotApps": []string{},
			"audio": map[string]any{"recordings": true, "calls": false, "voiceMessages": true}}})
	if got := string(set.Data["setHiddenCollections"]); got != `{"recordings":{"files":0,"hidden":true},"calls":{"files":1,"hidden":false},"voiceMessages":{"files":0,"hidden":true}}` {
		t.Fatalf("setHiddenCollections = %s %+v", got, set.Errors)
	}
	if store.Hidden().NudityFrom != media.DefaultNudityFrom {
		t.Errorf("nudity changed: %v", store.Hidden().NudityFrom)
	}
	want := `{"changes":[{"deleted":false,"hidden":false,"media":{"kind":"AUDIO","takenAt":"2025-07-26T15:23:07+03:00","endedAt":"2025-07-26T15:24:37+03:00","durationMs":90000,"call":{"contact":null,"number":"0501234567"}}}]}`
	if got := string(env.graphql(t, phone, feed, nil).Data["changes"]); got != want {
		t.Fatalf("shown: changes = %s", got)
	}
	if got := string(env.graphql(t, phone, `{ storageUsage { audio audioBytes bytes } serverInfo { audio } }`, nil).Data["storageUsage"]); got != `{"audio":1,"audioBytes":41,"bytes":41}` {
		t.Fatalf("storageUsage = %s", got)
	}

	req, _ := http.NewRequest("GET", env.server.URL+"/media/"+sha, nil)
	req.Header.Set("Range", "bytes=8-11")
	resp, err := phone.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(body) != "this" || resp.Header.Get("Content-Type") != "audio/mp4" {
		t.Fatalf("Range: %d %q %s", resp.StatusCode, body, resp.Header.Get("Content-Type"))
	}
	if resp, _ := phone.Get(env.server.URL + "/media/" + sha + "/preview?w=100&h=100"); resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("preview: %d", resp.StatusCode)
	}

	got := env.graphql(t, phone, `mutation($s: Sha256!) { setCall(sha256: $s, call: {contact: "דנה", number: "0501234567"}) { call { contact number } } }`,
		map[string]any{"s": sha})
	if s := string(got.Data["setCall"]); s != `{"call":{"contact":"דנה","number":"0501234567"}}` {
		t.Fatalf("setCall = %s %+v", s, got.Errors)
	}
	if s := string(env.graphql(t, phone, feed, nil).Data["changes"]); !strings.Contains(s, `"contact":"דנה"`) {
		t.Fatalf("after setCall: changes = %s", s)
	}
}
