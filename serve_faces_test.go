package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/faces"
	"github.com/journal/server-go/media"
)

func TestFaceSearchIsOffWithoutModels(t *testing.T) {
	env := newTestEnv(t)
	phone := env.enrolledClient(t)
	sha := env.uploadPhoto(t, phone, []byte("a face"), "A.jpg")
	resp := env.graphql(t, phone, `{ people { model } }`, nil)
	if len(resp.Errors) == 0 || resp.Errors[0].Message != "face search is off on this server" {
		t.Fatalf("people = %s %+v", resp.Data["people"], resp.Errors)
	}
	resp = env.graphql(t, phone, `query($s: Sha256!) { media(sha256: $s) { faces { id } } }`, map[string]any{"s": sha})
	if got := string(resp.Data["media"]); got != `{"faces":[]}` {
		t.Fatalf("media = %s %+v", got, resp.Errors)
	}
	resp = env.graphql(t, phone, `{ searchMedia(people: ["p1"]) { total } }`, nil)
	if len(resp.Errors) == 0 {
		t.Fatal("searched by people with face search off")
	}
}

// personFace is a made-up face of person n: close to the n-th direction, a little different each time (variant).
func personFace(n, variant int) faces.Face {
	v := make([]float32, faces.Dim)
	v[n] = 1
	v[10+variant] = 0.3
	var sum float32
	for _, x := range v {
		sum += x * x
	}
	for i := range v {
		v[i] /= float32(math.Sqrt(float64(sum)))
	}
	return faces.Face{Box: [4]float32{0.1 + 0.4*float32(n), 0.2, 0.2, 0.3}, Score: 0.95, Vector: v}
}

func TestPeopleAreGroupedNamedAndSearched(t *testing.T) {
	// The photos are uploaded to one server; their faces (made up: the models aren't needed) are written into the
	// data folder; a second server on the same folder serves them.
	first := newTestEnv(t)
	uploader := first.enrolledClient(t)
	var mom, dad []string
	for i := range 4 {
		mom = append(mom, first.uploadPhoto(t, uploader, []byte(fmt.Sprintf("mom %d", i)), fmt.Sprintf("MOM%d.jpg", i)))
		time.Sleep(time.Millisecond) // the last uploaded is the last taken
	}
	for i := range 3 {
		dad = append(dad, first.uploadPhoto(t, uploader, []byte(fmt.Sprintf("dad %d", i)), fmt.Sprintf("DAD%d.jpg", i)))
	}
	both := first.uploadPhoto(t, uploader, []byte("mom and dad"), "BOTH.jpg")
	private := first.uploadPhoto(t, uploader, []byte("mom in bed"), "BED.jpg")
	stranger := first.uploadPhoto(t, uploader, []byte("someone"), "SOMEONE.jpg")
	first.graphql(t, uploader, `mutation($h: [Sha256!]!) { setPrivate(sha256s: $h, private: true) { sha256 } }`,
		map[string]any{"h": []string{private}})

	index, err := faces.OpenIndex(first.dataFolder, fakeFaceModel)
	if err != nil {
		t.Fatal(err)
	}
	for i, sha := range mom {
		index.Add(sha, []faces.Face{personFace(0, i)})
	}
	for i, sha := range dad {
		index.Add(sha, []faces.Face{personFace(1, i)})
	}
	index.Add(both, []faces.Face{personFace(0, 5), personFace(1, 5)})
	index.Add(private, []faces.Face{personFace(0, 6)})
	index.Add(stranger, []faces.Face{personFace(2, 0)})
	index.Close()

	env := newTestEnv(t, func(c *auth.AppConfig) {
		c.Users[0].DataFolder = first.dataFolder
		c.FaceModel = fakeFaceModel
	})
	phone := env.enrolledClient(t)
	type person struct {
		ID    string
		Name  *string
		Files int
		Faces int
		Cover struct{ ID, Sha256 string }
	}
	people := func(query string) []person {
		t.Helper()
		resp := env.graphql(t, phone, `{ people`+query+` { people { id name files faces cover { id sha256 } } read total model } }`, nil)
		var got struct {
			People      []person
			Read, Total int
			Model       string
		}
		if err := json.Unmarshal(resp.Data["people"], &got); err != nil {
			t.Fatalf("people = %s %+v", resp.Data["people"], resp.Errors)
		}
		if got.Read != got.Total || got.Model != fakeFaceModel {
			t.Fatalf("read %d of %d with %s", got.Read, got.Total, got.Model)
		}
		return got.People
	}

	// Two groups (the stranger is alone), the most files first; the private photo counts only when asked for.
	groups := people("")
	if len(groups) != 2 || groups[0].Files != 5 || groups[1].Files != 4 || groups[0].Name != nil {
		t.Fatalf("groups: %+v", groups)
	}
	if withPrivate := people("(includePrivate: true)"); withPrivate[0].Files != 6 {
		t.Fatalf("with private files: %+v", withPrivate)
	}
	if named := people("(includeGroups: false)"); len(named) != 0 {
		t.Fatalf("no one is named yet: %+v", named)
	}

	// Naming the group makes Mom.
	resp := env.graphql(t, phone, `mutation($id: ID!) { namePerson(id: $id, name: "Mom") {
		... on Person { id name files } ... on NotFound { reason } } }`, map[string]any{"id": groups[0].ID})
	var named struct {
		ID    string
		Name  string
		Files int
	}
	if err := json.Unmarshal(resp.Data["namePerson"], &named); err != nil || named.Name != "Mom" || !strings.HasPrefix(named.ID, "p") {
		t.Fatalf("namePerson = %s %+v", resp.Data["namePerson"], resp.Errors)
	}
	momID := named.ID
	if data, err := os.ReadFile(filepath.Join(env.dataFolder, "people.json")); err != nil || !strings.Contains(string(data), `"Mom"`) {
		t.Fatalf("people.json: %s %v", data, err)
	}
	if list := people(""); list[0].ID != momID || *list[0].Name != "Mom" {
		t.Fatalf("named people come first: %+v", list)
	}
	resp = env.graphql(t, phone, `mutation { namePerson(id: "gnone", name: "X") { ... on NotFound { id reason } } }`, nil)
	if !strings.Contains(string(resp.Data["namePerson"]), `"id":"gnone"`) {
		t.Fatalf("naming a group that isn't there = %s %+v", resp.Data["namePerson"], resp.Errors)
	}

	// A search by Mom alone: her photos, the last taken first, without the private one.
	search := func(variables map[string]any) []string {
		t.Helper()
		resp := env.graphql(t, phone, `query($p: [ID!], $t: String) { searchMedia(people: $p, text: $t) { hits { media { sha256 } } } }`, variables)
		var got struct {
			Hits []struct{ Media struct{ Sha256 string } }
		}
		if err := json.Unmarshal(resp.Data["searchMedia"], &got); err != nil {
			t.Fatalf("searchMedia = %s %+v", resp.Data["searchMedia"], resp.Errors)
		}
		var shas []string
		for _, h := range got.Hits {
			shas = append(shas, h.Media.Sha256)
		}
		return shas
	}
	want := slices.Clone(append(mom, both))
	slices.Reverse(want)
	if got := search(map[string]any{"p": []string{momID}}); !slices.Equal(got, want) {
		t.Errorf("Mom's photos: %v, want %v", got, want)
	}
	if got := search(map[string]any{"p": []string{momID, groups[1].ID}}); !slices.Equal(got, []string{both}) {
		t.Errorf("photos of both: %v", got)
	}
	resp = env.graphql(t, phone, `{ searchMedia(people: ["pgone"]) { total } }`, nil)
	if len(resp.Errors) == 0 {
		t.Error("searched for someone who isn't there")
	}
	resp = env.graphql(t, phone, `{ searchMedia(text: "mom", people: []) { total } }`, nil)
	if len(resp.Errors) == 0 || resp.Errors[0].Message != "text search is off on this server" {
		t.Errorf("text search = %+v", resp.Errors)
	}
	resp = env.graphql(t, phone, `{ searchMedia { total } }`, nil)
	if len(resp.Errors) == 0 {
		t.Error("searched for nothing")
	}

	// The faces in a photo say whom they are.
	resp = env.graphql(t, phone, `query($s: Sha256!) { media(sha256: $s) { faces { id personId personName box { x w } } } }`,
		map[string]any{"s": both})
	var media struct {
		Faces []struct {
			ID         string
			PersonID   *string
			PersonName *string
			Box        struct{ X, W float64 }
		}
	}
	if err := json.Unmarshal(resp.Data["media"], &media); err != nil || len(media.Faces) != 2 {
		t.Fatalf("faces = %s %+v", resp.Data["media"], resp.Errors)
	}
	var momFace string
	for _, f := range media.Faces {
		if f.PersonName != nil && *f.PersonName == "Mom" {
			momFace = f.ID
		}
	}
	if momFace == "" {
		t.Fatalf("no face of Mom in %+v", media.Faces)
	}

	// More photos of her face: her others first, not the private one, not the photo itself.
	resp = env.graphql(t, phone, `query($f: ID!) { similarFaces(face: $f, limit: 4) { score face { personId } media { sha256 } } }`,
		map[string]any{"f": momFace})
	var hits []struct {
		Score float64
		Face  struct{ PersonID string }
		Media struct{ Sha256 string }
	}
	if err := json.Unmarshal(resp.Data["similarFaces"], &hits); err != nil || len(hits) != 4 {
		t.Fatalf("similarFaces = %s %+v", resp.Data["similarFaces"], resp.Errors)
	}
	for _, h := range hits {
		if !slices.Contains(mom, h.Media.Sha256) || h.Face.PersonID != momID || h.Score < 0.36 {
			t.Errorf("a face like Mom's: %+v", h)
		}
	}

	// A face moved away from her stays away; forgetting her puts her faces back in a group.
	resp = env.graphql(t, phone, `mutation($f: ID!) { moveFace(face: $f, to: null) { ... on Face { id personId } } }`,
		map[string]any{"f": mom[0] + ":0"})
	if got := string(resp.Data["moveFace"]); got != `{"id":"`+mom[0]+`:0","personId":null}` {
		t.Fatalf("moveFace = %s %+v", got, resp.Errors)
	}
	if list := people(""); list[0].Files != 4 {
		t.Errorf("Mom after a face was moved away: %+v", list[0])
	}
	resp = env.graphql(t, phone, `mutation($id: ID!) { forgetPerson(id: $id) }`, map[string]any{"id": momID})
	if string(resp.Data["forgetPerson"]) != "true" {
		t.Fatalf("forgetPerson = %s %+v", resp.Data["forgetPerson"], resp.Errors)
	}
	resp = env.graphql(t, phone, `mutation($id: ID!) { forgetPerson(id: $id) }`, map[string]any{"id": momID})
	if string(resp.Data["forgetPerson"]) != "false" {
		t.Fatalf("forgetting twice = %s %+v", resp.Data["forgetPerson"], resp.Errors)
	}
	if list := people(""); len(list) != 2 || list[0].Name != nil {
		t.Errorf("after forgetting Mom: %+v", list)
	}
}

// A device kept from private files doesn't see them in people, faces or searches, even when it asks.
func TestWithheldDevicesDontSeePrivateFaces(t *testing.T) {
	first := newTestEnv(t)
	uploader := first.enrolledClient(t)
	var mom []string
	for i := range 3 {
		mom = append(mom, first.uploadPhoto(t, uploader, []byte(fmt.Sprintf("mom %d", i)), fmt.Sprintf("MOM%d.jpg", i)))
	}
	first.graphql(t, uploader, `mutation($h: [Sha256!]!) { setPrivate(sha256s: $h, private: true) { sha256 } }`,
		map[string]any{"h": []string{mom[2]}})
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
	tablet := env.enrolledClient(t)
	devices := env.enrollment.Registry.List("hen")
	if _, err := env.enrollment.Registry.SetWithholdPrivate(devices[1].Serial, true); err != nil {
		t.Fatal(err)
	}
	resp := env.graphql(t, phone, `{ people(includePrivate: true) { people { files } } }`, nil)
	if got := string(resp.Data["people"]); got != `{"people":[{"files":3}]}` {
		t.Fatalf("phone's people = %s %+v", got, resp.Errors)
	}
	// Three faces, but one is in a file the tablet may not see: a group of two is not shown.
	resp = env.graphql(t, tablet, `{ people(includePrivate: true) { people { files } } }`, nil)
	if got := string(resp.Data["people"]); got != `{"people":[]}` {
		t.Fatalf("tablet's people = %s %+v", got, resp.Errors)
	}
	resp = env.graphql(t, tablet, `query($f: ID!) { similarFaces(face: $f, includePrivate: true) { media { sha256 } } }`,
		map[string]any{"f": mom[0] + ":0"})
	if got := string(resp.Data["similarFaces"]); got != `[{"media":{"sha256":"`+mom[1]+`"}}]` {
		t.Fatalf("tablet's similarFaces = %s %+v", got, resp.Errors)
	}
	resp = env.graphql(t, tablet, `query($f: ID!) { similarFaces(face: $f) { score } }`, map[string]any{"f": mom[2] + ":0"})
	if len(resp.Errors) == 0 {
		t.Fatal("the tablet asked for faces like one in a file it may not see")
	}
}

// The real models read uploaded photos (from $JOURNAL_FACE_TESTDATA: none are kept in the repository) through their
// 1280 px previews, and find their faces.
func TestFacesAreFoundInUploadedPhotos(t *testing.T) {
	models, _ := filepath.Abs(auth.DefaultFaceModel)
	if _, err := os.Stat(models); err != nil {
		t.Skip("no face models (go run ./scripts/bundle-onnx)")
	}
	if _, _, err := media.FindVips(""); err != nil {
		t.Skip("libvips not installed")
	}
	dir := os.Getenv("JOURNAL_FACE_TESTDATA")
	photos, _ := filepath.Glob(filepath.Join(dir, "*.jpg"))
	if len(photos) == 0 {
		t.Skip("JOURNAL_FACE_TESTDATA is not a folder of JPEG photos with faces")
	}
	env := newTestEnv(t, func(c *auth.AppConfig) { c.FaceModel = models; c.SearchThreads = 4 })
	phone := env.enrolledClient(t)
	var shas []string
	for _, path := range photos {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		shas = append(shas, env.uploadPhoto(t, phone, data, filepath.Base(path)))
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { env.reader.Run(ctx); close(done) }()
	t.Cleanup(func() { stop(); <-done })
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(50 * time.Millisecond) {
		if read, _ := env.faces.Counts("hen", func(media.Item) bool { return false }); read == len(shas) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the photos were not read in a minute")
		}
	}
	for i, sha := range shas {
		resp := env.graphql(t, phone, `query($s: Sha256!) { media(sha256: $s) { faces { id score } } }`, map[string]any{"s": sha})
		var got struct{ Faces []struct{ Score float64 } }
		if err := json.Unmarshal(resp.Data["media"], &got); err != nil || len(got.Faces) == 0 {
			t.Errorf("%s: faces = %s %+v", filepath.Base(photos[i]), resp.Data["media"], resp.Errors)
		}
		t.Logf("%s: %d faces", filepath.Base(photos[i]), len(got.Faces))
	}
}
