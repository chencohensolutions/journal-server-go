package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/search"
)

// indexesEnv uploads n photos to one server, has prepare write what the indexes would have read into the data
// folder, and starts a second server on that folder with fake OCR, similar and aesthetic models.
func indexesEnv(t *testing.T, n int, prepare func(dataFolder string, shas []string)) (*testEnv, []string) {
	t.Helper()
	first := newTestEnv(t)
	uploader := first.enrolledClient(t)
	var shas []string
	for i := range n {
		shas = append(shas, first.uploadPhoto(t, uploader, []byte(fmt.Sprintf("photo %d", i)), fmt.Sprintf("IMG%d.jpg", i)))
	}
	prepare(first.dataFolder, shas)
	env := newTestEnv(t, func(c *auth.AppConfig) {
		c.Users[0].DataFolder = first.dataFolder
		c.OCRModel, c.SimilarModel, c.AestheticModel = fakeModel, fakeModel, fakeModel
	})
	return env, shas
}

func writeVectors(t *testing.T, path string, dim int, vectors map[string][]float32) {
	t.Helper()
	index, err := search.OpenVectors(path, dim)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	for sha, v := range vectors {
		if err := index.Add(sha, v); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTextInPhotosIsFoundBySearch(t *testing.T) {
	env, shas := indexesEnv(t, 3, func(dataFolder string, shas []string) {
		lines := map[string]string{
			shas[0]: `[{"text":"קבלה מספר 4521","box":[0.1,0.1,0.5,0.05],"score":0.98}]`,
			shas[1]: `[{"text":"תל אביב יפו","box":[0.2,0.3,0.4,0.05],"score":0.95},{"text":"בקבלה","box":[0.2,0.5,0.2,0.05],"score":0.9}]`,
			shas[2]: `[]`,
		}
		path := filepath.Join(dataFolder, ".cache", "ocr", fakeModel+".jsonl")
		os.MkdirAll(filepath.Dir(path), 0o755)
		var data []byte
		for _, sha := range shas {
			data = append(data, fmt.Sprintf(`{"sha256":%q,"lines":%s}`+"\n", sha, lines[sha])...)
		}
		os.WriteFile(path, data, 0o644)
	})
	phone := env.enrolledClient(t)

	search := func(text string) []string {
		t.Helper()
		resp := env.graphql(t, phone, `query($t: String) { searchMedia(text: $t) { hits { score media { sha256 } } indexed total } }`, map[string]any{"t": text})
		var got struct {
			Hits []struct {
				Score float64
				Media struct{ Sha256 string }
			}
			Indexed, Total int
		}
		if err := json.Unmarshal(resp.Data["searchMedia"], &got); err != nil {
			t.Fatalf("searchMedia(%q) = %s %+v", text, resp.Data["searchMedia"], resp.Errors)
		}
		var found []string
		for _, h := range got.Hits {
			if h.Score != 1 {
				t.Errorf("a photo found by its text scores %v", h.Score)
			}
			found = append(found, h.Media.Sha256)
		}
		if got.Indexed != 3 || got.Total != 3 {
			t.Errorf("read %d of %d", got.Indexed, got.Total)
		}
		return found
	}
	// Text search (SigLIP) is off here: the words in photos are found alone.
	if got := search("קבלה"); len(got) != 2 || !slices.Contains(got, shas[0]) || !slices.Contains(got, shas[1]) {
		t.Errorf("קבלה: %v", got)
	}
	if got := search("תל אביב"); !slices.Equal(got, []string{shas[1]}) {
		t.Errorf("תל אביב: %v", got)
	}
	if got := search("4521"); !slices.Equal(got, []string{shas[0]}) {
		t.Errorf("4521: %v", got)
	}

	resp := env.graphql(t, phone, `query($s: Sha256!) { media(sha256: $s) { text { text score box { x w } } } }`, map[string]any{"s": shas[1]})
	if got := string(resp.Data["media"]); got != `{"text":[{"text":"תל אביב יפו","score":0.95,"box":{"x":0.2,"w":0.4}},{"text":"בקבלה","score":0.9,"box":{"x":0.2,"w":0.2}}]}` {
		t.Errorf("media.text = %s %+v", got, resp.Errors)
	}
	resp = env.graphql(t, phone, `{ serverInfo { textInPhotos similar aesthetic search } }`, nil)
	if got := string(resp.Data["serverInfo"]); got != `{"textInPhotos":true,"similar":true,"aesthetic":true,"search":false}` {
		t.Errorf("serverInfo = %s", got)
	}

	// A private photo is found only when the app asks for private media.
	env.graphql(t, phone, `mutation($h: [Sha256!]!) { setPrivate(sha256s: $h, private: true) { sha256 } }`, map[string]any{"h": []string{shas[0]}})
	resp = env.graphql(t, phone, `{ searchMedia(text: "4521") { hits { media { sha256 } } } }`, nil)
	if got := string(resp.Data["searchMedia"]); got != `{"hits":[]}` {
		t.Errorf("private photo found: %s", got)
	}
}

func TestSimilarPhotosAndDuplicates(t *testing.T) {
	env, shas := indexesEnv(t, 4, func(dataFolder string, shas []string) {
		// 0 and 1 are copies, 2 is the same scene, 3 something else.
		writeVectors(t, filepath.Join(dataFolder, ".cache", "similar", fakeModel+".vectors"), 2, map[string][]float32{
			shas[0]: {1, 0}, shas[1]: {0.99, 0.141}, shas[2]: {0.9, 0.436}, shas[3]: {0, 1},
		})
		// The copy looks better than the original.
		writeVectors(t, filepath.Join(dataFolder, ".cache", "aesthetic", fakeModel+".vectors"), 1, map[string][]float32{
			shas[0]: {4.5}, shas[1]: {5.25},
		})
	})
	phone := env.enrolledClient(t)

	resp := env.graphql(t, phone, `query($s: Sha256!) { similarMedia(sha256: $s, limit: 2) { score media { sha256 } } }`, map[string]any{"s": shas[0]})
	var hits []struct {
		Score float64
		Media struct{ Sha256 string }
	}
	if err := json.Unmarshal(resp.Data["similarMedia"], &hits); err != nil || len(hits) != 2 {
		t.Fatalf("similarMedia = %s %+v", resp.Data["similarMedia"], resp.Errors)
	}
	if hits[0].Media.Sha256 != shas[1] || hits[1].Media.Sha256 != shas[2] || hits[0].Score < hits[1].Score {
		t.Errorf("most like the first: %+v", hits)
	}

	resp = env.graphql(t, phone, `{ duplicates { score media { sha256 aesthetic } } }`, nil)
	var groups []struct {
		Score float64
		Media []struct {
			Sha256    string
			Aesthetic *float64
		}
	}
	if err := json.Unmarshal(resp.Data["duplicates"], &groups); err != nil {
		t.Fatalf("duplicates = %s %+v", resp.Data["duplicates"], resp.Errors)
	}
	if len(groups) != 1 || len(groups[0].Media) != 2 || groups[0].Score < 0.96 {
		t.Fatalf("duplicates: %+v", groups)
	}
	if g := groups[0].Media; g[0].Sha256 != shas[1] || *g[0].Aesthetic != 5.25 || g[1].Sha256 != shas[0] {
		t.Errorf("the best looking copy comes first: %+v", g)
	}

	// Hiding one of the copies makes it no group.
	env.graphql(t, phone, `mutation($h: [Sha256!]!) { setPrivate(sha256s: $h, private: true) { sha256 } }`, map[string]any{"h": []string{shas[1]}})
	resp = env.graphql(t, phone, `{ duplicates { score } }`, nil)
	if got := string(resp.Data["duplicates"]); got != `[]` {
		t.Errorf("duplicates with one copy private: %s", got)
	}
	resp = env.graphql(t, phone, `query($s: Sha256!) { media(sha256: $s) { aesthetic } }`, map[string]any{"s": shas[3]})
	if got := string(resp.Data["media"]); got != `{"aesthetic":null}` {
		t.Errorf("not scored: %s", got)
	}
}

func TestSimilarPhotosAreOffWithoutAModel(t *testing.T) {
	env := newTestEnv(t)
	phone := env.enrolledClient(t)
	sha := env.uploadPhoto(t, phone, []byte("a photo"), "A.jpg")
	resp := env.graphql(t, phone, `query($s: Sha256!) { similarMedia(sha256: $s) { score } }`, map[string]any{"s": sha})
	if len(resp.Errors) == 0 || resp.Errors[0].Message != "similar photos are off on this server" {
		t.Fatalf("similarMedia = %+v", resp.Errors)
	}
	resp = env.graphql(t, phone, `query($s: Sha256!) { media(sha256: $s) { text { text } aesthetic } }`, map[string]any{"s": sha})
	if got := string(resp.Data["media"]); got != `{"text":[],"aesthetic":null}` {
		t.Fatalf("media = %s", got)
	}
}
