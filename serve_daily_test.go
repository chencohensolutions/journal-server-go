package main

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/faces"
	"github.com/journal/server-go/search"
)

// adminConfig is config pointed at a test admin server that runs refresh on indexesUpdated.
func adminConfig(t *testing.T, env *testEnv) auth.AppConfig {
	t.Helper()
	admin := httptest.NewServer(newAdminMux(env.config, env.enrollment, "secret", env.refreshIndexes))
	t.Cleanup(admin.Close)
	tokenFile := filepath.Join(t.TempDir(), "admin.token")
	os.WriteFile(tokenFile, []byte("secret\n"), 0o600)
	config := env.config
	config.AdminAddr = strings.TrimPrefix(admin.URL, "http://")
	config.AdminTokenFile = tokenFile
	return config
}

// With "index": "daily" the server loads no reading model and runs no reader: another process (journal-server
// index) writes the caches, and the server answers from them once told (indexesUpdated).
func TestADailyServerAnswersFromWhatTheIndexerWrote(t *testing.T) {
	first := newTestEnv(t)
	uploader := first.enrolledClient(t)
	var shas []string
	for i := range 4 {
		shas = append(shas, first.uploadPhoto(t, uploader, []byte(fmt.Sprintf("photo %d", i)), fmt.Sprintf("IMG%d.jpg", i)))
	}
	// Model folders named as the indexer's models (only their names count here); text search off.
	models := t.TempDir()
	folder := func(name string) string {
		dir := filepath.Join(models, name)
		os.MkdirAll(dir, 0o755)
		return dir
	}
	env := newTestEnv(t, func(c *auth.AppConfig) {
		c.Users[0].DataFolder = first.dataFolder
		c.Index = auth.IndexDaily
		c.SearchModel = filepath.Join(models, "none")
		c.FaceModel, c.OCRModel = folder("faces-m"), folder("ocr-m")
		c.SimilarModel, c.AestheticModel = folder("similar-m"), folder("aesthetic-m")
	})
	if env.reader != nil {
		t.Fatal("a daily server reads files itself")
	}
	phone := env.enrolledClient(t)
	resp := env.graphql(t, phone, `{ searchMedia(text: "קבלה") { hits { media { sha256 } } } }`, nil)
	if got := string(resp.Data["searchMedia"]); got != `{"hits":[]}` {
		t.Fatalf("before the run: %s %+v", got, resp.Errors)
	}

	// The indexer's run: faces of one person in three photos, text in the first, vectors and scores.
	root := first.dataFolder
	facesIndex, err := faces.OpenIndex(root, "faces-m")
	if err != nil {
		t.Fatal(err)
	}
	for i, sha := range shas[:3] {
		facesIndex.Add(sha, []faces.Face{personFace(0, i)})
	}
	facesIndex.Add(shas[3], nil)
	facesIndex.Close()
	os.MkdirAll(filepath.Join(root, ".cache", "ocr"), 0o755)
	os.WriteFile(filepath.Join(root, ".cache", "ocr", "ocr-m.jsonl"),
		[]byte(fmt.Sprintf(`{"sha256":%q,"lines":[{"text":"קבלה מספר 4521","box":[0,0,1,0.1],"score":0.9}]}`+"\n", shas[0])), 0o644)
	writeVectors(t, filepath.Join(root, ".cache", "similar", "similar-m.vectors"), 2, map[string][]float32{
		shas[0]: {1, 0}, shas[1]: {0.99, 0.141}, shas[2]: {0, 1}, shas[3]: {0.6, 0.8},
	})
	writeVectors(t, filepath.Join(root, ".cache", "aesthetic", "aesthetic-m.vectors"), 1, map[string][]float32{shas[0]: {5.5}})

	var told struct {
		IndexesUpdated bool `json:"indexesUpdated"`
	}
	if err := adminQuery(adminConfig(t, env), `mutation { indexesUpdated }`, nil, &told); err != nil || !told.IndexesUpdated {
		t.Fatalf("indexesUpdated: %v %v", told, err)
	}
	resp = env.graphql(t, phone, `{ searchMedia(text: "קבלה") { hits { media { sha256 aesthetic } } } }`, nil)
	if got := string(resp.Data["searchMedia"]); got != `{"hits":[{"media":{"sha256":"`+shas[0]+`","aesthetic":5.5}}]}` {
		t.Errorf("text in photos after the run: %s %+v", got, resp.Errors)
	}
	resp = env.graphql(t, phone, `{ people { people { files } read total } }`, nil)
	if got := string(resp.Data["people"]); got != `{"people":[{"files":3}],"read":4,"total":4}` {
		t.Errorf("people after the run: %s %+v", got, resp.Errors)
	}
	resp = env.graphql(t, phone, `query($s: Sha256!) { similarMedia(sha256: $s, limit: 1) { media { sha256 } } }`, map[string]any{"s": shas[0]})
	if got := string(resp.Data["similarMedia"]); got != `[{"media":{"sha256":"`+shas[1]+`"}}]` {
		t.Errorf("similar after the run: %s %+v", got, resp.Errors)
	}
	resp = env.graphql(t, phone, `{ serverInfo { search faces textInPhotos similar aesthetic } }`, nil)
	if got := string(resp.Data["serverInfo"]); got != `{"search":false,"faces":true,"textInPhotos":true,"similar":true,"aesthetic":true}` {
		t.Errorf("serverInfo: %s", got)
	}
	// The caches were never written by the server: nothing cut or added.
	if _, err := search.OpenVectorsReadOnly(filepath.Join(root, ".cache", "similar", "similar-m.vectors"), 2); err != nil {
		t.Error(err)
	}
}

// A server that reads files itself has nothing to refresh: indexesUpdated says so.
func TestAServerThatReadsItselfIgnoresIndexesUpdated(t *testing.T) {
	env := newTestEnv(t)
	var told struct {
		IndexesUpdated bool `json:"indexesUpdated"`
	}
	if err := adminQuery(adminConfig(t, env), `mutation { indexesUpdated }`, nil, &told); err != nil || told.IndexesUpdated {
		t.Fatalf("indexesUpdated: %v %v", told, err)
	}
}

func TestTheDailyRunIsAtTheNextIndexAt(t *testing.T) {
	now := mustTime(t, "2026-10-23T14:00:00")
	for at, want := range map[string]string{"03:00": "2026-10-24T03:00:00", "14:30": "2026-10-23T14:30:00", "14:00": "2026-10-24T14:00:00"} {
		if got := nextRun(at, now); !got.Equal(mustTime(t, want)) {
			t.Errorf("%s from 14:00: %v, want %s", at, got, want)
		}
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02T15:04:05", s, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
