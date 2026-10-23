package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/faces"
	"github.com/journal/server-go/localindex"
)

// localStream is a stream of GET /index/{name}, read as a phone reads it.
type localStream struct {
	head        localindex.StreamHead
	records     []string // the sha256 of each record, in order
	drops       []string
	checkpoints []string
	end         string
	ended       bool
	gzipped     bool
}

func (e *testEnv) localStream(t *testing.T, client *http.Client, name, cursor string) localStream {
	t.Helper()
	resp, err := client.Get(e.server.URL + "/index/" + name + "?cursor=" + url.QueryEscape(cursor))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/index/%s: %d", name, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	s := localStream{gzipped: resp.Uncompressed}
	line, rest, _ := bytes.Cut(body, []byte("\n"))
	if err := json.Unmarshal(line, &s.head); err != nil {
		t.Fatalf("head %q: %v", line, err)
	}
	for len(rest) > 0 {
		if len(rest) < 5 {
			t.Fatalf("a frame cut short: %q", rest)
		}
		kind, n := rest[0], int(binary.LittleEndian.Uint32(rest[1:5]))
		payload := rest[5 : 5+n]
		rest = rest[5+n:]
		switch kind {
		case localindex.FrameRecord:
			if s.head.Format == localindex.FormatJSONL {
				var rec struct{ Sha256 string }
				json.Unmarshal(payload, &rec)
				s.records = append(s.records, rec.Sha256)
			} else {
				s.records = append(s.records, hex.EncodeToString(payload[:32]))
			}
		case localindex.FrameDrop:
			s.drops = append(s.drops, hex.EncodeToString(payload))
		case localindex.FrameCheckpoint:
			s.checkpoints = append(s.checkpoints, string(payload))
		case localindex.FrameEnd:
			s.end, s.ended = string(payload), true
		default:
			t.Fatalf("frame %q", kind)
		}
	}
	if !s.ended {
		t.Fatal("no end frame")
	}
	return s
}

// localEnv is a daily server (the indexes written by another process, as journal-server index does) over four
// uploaded photos, with similar vectors of the first three, the faces of one person in them, and text in the first.
func localEnv(t *testing.T) (*testEnv, []string) {
	t.Helper()
	first := newTestEnv(t)
	uploader := first.enrolledClient(t)
	var shas []string
	for i := range 4 {
		shas = append(shas, first.uploadPhoto(t, uploader, []byte(fmt.Sprintf("photo %d", i)), fmt.Sprintf("IMG%d.jpg", i)))
	}
	root := first.dataFolder
	writeVectors(t, filepath.Join(root, ".cache", "similar", "similar-m.vectors"), 2, map[string][]float32{
		shas[0]: {1, 0}, shas[1]: {0, 1}, shas[2]: {0.6, 0.8},
	})
	facesIndex, err := faces.OpenIndex(root, "faces-m")
	if err != nil {
		t.Fatal(err)
	}
	for i, sha := range shas[:3] {
		facesIndex.Add(sha, []faces.Face{personFace(0, i)})
	}
	facesIndex.Close()
	os.MkdirAll(filepath.Join(root, ".cache", "ocr"), 0o755)
	os.WriteFile(filepath.Join(root, ".cache", "ocr", "ocr-m.jsonl"),
		[]byte(fmt.Sprintf(`{"sha256":%q,"lines":[{"text":"קבלה מספר 4521","box":[0,0,1,0.1],"score":0.9}]}`+"\n", shas[0])), 0o644)

	models := t.TempDir()
	folder := func(name string) string {
		dir := filepath.Join(models, name)
		os.MkdirAll(dir, 0o755)
		return dir
	}
	env := newTestEnv(t, func(c *auth.AppConfig) {
		c.Users[0].DataFolder = root
		c.Index = auth.IndexDaily
		c.SearchModel = filepath.Join(models, "none")
		c.FaceModel, c.OCRModel = folder("faces-m"), folder("ocr-m")
		c.SimilarModel, c.AestheticModel = folder("similar-m"), filepath.Join(models, "none")
	})
	return env, shas
}

func TestAPhoneKeepsAnIndexByWhatWasAdded(t *testing.T) {
	env, shas := localEnv(t)
	phone := env.enrolledClient(t)
	path := filepath.Join(env.dataFolder, ".cache", "similar", "similar-m.vectors")

	all := env.localStream(t, phone, "similar", "")
	slices.Sort(all.records)
	want := slices.Sorted(slices.Values(shas[:3]))
	if !all.head.Reset || all.head.Model != "similar-m" || all.head.Dim != 2 || !slices.Equal(all.records, want) {
		t.Fatalf("from the start: %+v, want the first three", all)
	}
	if again := env.localStream(t, phone, "similar", all.end); again.head.Reset || len(again.records) != 0 || len(again.drops) != 0 {
		t.Fatalf("with nothing new: %+v", again)
	}
	// A stream that breaks goes on from its last checkpoint (here one after every record).
	defer func(every int64) { localindex.CheckpointEvery = every }(localindex.CheckpointEvery)
	localindex.CheckpointEvery = 1
	pieces := env.localStream(t, phone, "similar", "")
	if len(pieces.checkpoints) != 3 {
		t.Fatalf("checkpoints: %+v", pieces)
	}
	if rest := env.localStream(t, phone, "similar", pieces.checkpoints[0]); rest.head.Reset || !slices.Equal(rest.records, pieces.records[1:]) {
		t.Fatalf("from the first checkpoint: %+v, want the two after it", rest)
	}
	localindex.CheckpointEvery = 4 << 20

	// The indexer reads the fourth photo: only it comes.
	writeVectors(t, path, 2, map[string][]float32{shas[3]: {0.8, 0.6}})
	added := env.localStream(t, phone, "similar", all.end)
	if added.head.Reset || !slices.Equal(added.records, shas[3:]) {
		t.Fatalf("after one more: %+v", added)
	}

	// The file started over (deleted and read again, in another order): everything again, the phone dropping what it had.
	os.Remove(path)
	writeVectors(t, path, 2, map[string][]float32{shas[3]: {0.8, 0.6}, shas[2]: {0.6, 0.8}, shas[1]: {0, 1}, shas[0]: {1, 0}})
	if over := env.localStream(t, phone, "similar", added.end); !over.head.Reset || len(over.records) != 4 {
		t.Fatalf("after the file started over: %+v", over)
	}
	if bad := env.localStream(t, phone, "similar", "not a cursor"); !bad.head.Reset || len(bad.records) != 4 {
		t.Fatalf("with a cursor it can't read: %+v", bad)
	}

	// Without a certificate, nothing.
	for _, p := range []string{"/index/similar", "/index/people", "/models/search/tokenizer.model"} {
		resp, err := env.client(nil).Get(env.server.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous %s: %d", p, resp.StatusCode)
		}
	}
	if resp, _ := phone.Get(env.server.URL + "/index/nothing"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an index that isn't: %d", resp.StatusCode)
	}
}

// A private file is never sent to a device kept from them; one marked private later is taken back, and one no longer
// private is sent again, without the rest of the index coming again.
func TestAPhoneIndexFollowsWhatTheDeviceMaySee(t *testing.T) {
	env, shas := localEnv(t)
	phone := env.enrolledClient(t)
	tablet := env.enrolledClient(t)
	devices := env.enrollment.Registry.List("hen")
	if _, err := env.enrollment.Registry.SetWithholdPrivate(devices[1].Serial, true); err != nil {
		t.Fatal(err)
	}
	before := env.localStream(t, tablet, "similar", "")
	setPrivate := func(private bool) {
		t.Helper()
		resp := env.graphql(t, phone, `mutation($h: [Sha256!]!, $p: Boolean!) { setPrivate(sha256s: $h, private: $p) { sha256 } }`,
			map[string]any{"h": []string{shas[0]}, "p": private})
		if len(resp.Errors) > 0 {
			t.Fatal(resp.Errors)
		}
	}
	setPrivate(true)
	taken := env.localStream(t, tablet, "similar", before.end)
	if !slices.Equal(taken.drops, shas[:1]) || len(taken.records) != 0 || taken.head.Reset {
		t.Fatalf("marked private: %+v, want the first dropped", taken)
	}
	if fresh := env.localStream(t, tablet, "similar", ""); len(fresh.records) != 2 || slices.Contains(fresh.records, shas[0]) {
		t.Fatalf("from the start: %+v, want it left out", fresh)
	}
	// The phone isn't kept from private files: it keeps it.
	if mine := env.localStream(t, phone, "similar", before.end); len(mine.drops) != 0 || !slices.Equal(mine.records, shas[:1]) {
		t.Fatalf("the phone's: %+v, want the first sent again", mine)
	}
	setPrivate(false)
	back := env.localStream(t, tablet, "similar", taken.end)
	if !slices.Equal(back.records, shas[:1]) || len(back.drops) != 0 {
		t.Fatalf("no longer private: %+v, want the first sent again", back)
	}
	if people := getPeople(t, env, tablet, ""); people.status != http.StatusOK || len(people.doc.People) != 1 || len(people.doc.People[0].Faces) != 3 {
		t.Fatalf("people: %+v", people)
	}
}

type peopleAnswer struct {
	status int
	etag   string
	doc    struct {
		V      int
		Model  string
		People []struct {
			ID    string
			Name  string
			Faces []string
		}
	}
}

func getPeople(t *testing.T, env *testEnv, client *http.Client, etag string) peopleAnswer {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, env.server.URL+"/index/people", nil)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	a := peopleAnswer{status: resp.StatusCode, etag: resp.Header.Get("ETag")}
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&a.doc); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func TestAPhoneKeepsFacesTextAndPeople(t *testing.T) {
	env, shas := localEnv(t)
	phone := env.enrolledClient(t)

	f := env.localStream(t, phone, "faces", "")
	if f.head.Format != localindex.FormatFaces || f.head.Dim != faces.Dim || len(f.records) != 3 {
		t.Fatalf("faces: %+v", f)
	}
	o := env.localStream(t, phone, "ocr", "")
	if o.head.Format != localindex.FormatJSONL || !o.gzipped || !slices.Equal(o.records, shas[:1]) {
		t.Fatalf("text in photos: %+v", o)
	}

	people := getPeople(t, env, phone, "")
	if people.status != http.StatusOK || people.etag == "" || people.doc.Model != "faces-m" || len(people.doc.People) != 1 ||
		len(people.doc.People[0].Faces) != 3 || !slices.Contains(people.doc.People[0].Faces, shas[0]+":0") {
		t.Fatalf("people: %+v", people)
	}
	if again := getPeople(t, env, phone, people.etag); again.status != http.StatusNotModified {
		t.Errorf("people unchanged: %d", again.status)
	}

	resp := env.graphql(t, phone, `{ serverInfo { localIndexes } localIndexes { name model format dim files bytes models { name } } }`, nil)
	if got := string(resp.Data["serverInfo"]); got != `{"localIndexes":true}` {
		t.Errorf("serverInfo: %s", got)
	}
	var manifest []struct {
		Name, Model, Format string
		Dim, Files          int
		Bytes               int64
		Models              []struct{ Name string }
	}
	if err := json.Unmarshal(resp.Data["localIndexes"], &manifest); err != nil || len(manifest) != 3 {
		t.Fatalf("localIndexes: %s %+v", resp.Data["localIndexes"], resp.Errors)
	}
	for _, x := range manifest {
		switch x.Name {
		case "faces":
			if x.Files != 3 || x.Dim != faces.Dim || x.Bytes != 3*(34+4*(15+faces.Dim)) || len(x.Models) != 0 {
				t.Errorf("faces: %+v", x)
			}
		case "ocr":
			if x.Files != 1 || x.Format != "jsonl" || x.Bytes == 0 {
				t.Errorf("ocr: %+v", x)
			}
		case "similar":
			if x.Files != 3 || x.Dim != 2 || x.Bytes != 3*(32+4*2) {
				t.Errorf("similar: %+v", x)
			}
		default:
			t.Errorf("an index that is off: %+v", x)
		}
	}
}
