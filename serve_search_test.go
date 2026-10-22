package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/media"
)

const searchQuery = `query($t: String!) { searchMedia(text: $t, limit: 10) { indexed total model hits { score set moment media { sha256 } } } }`

func TestSearchIsOffWithoutAModel(t *testing.T) {
	env := newTestEnv(t)
	resp := env.graphql(t, env.enrolledClient(t), searchQuery, map[string]any{"t": "a dog"})
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0].Message, "text search is off") {
		t.Fatalf("errors = %+v", resp.Errors)
	}
}

func TestSearchFindsPhotosByTheirDescription(t *testing.T) {
	model, _ := filepath.Abs(auth.DefaultSearchModel)
	if _, err := os.Stat(model); err != nil {
		t.Skip("no search model (go run ./scripts/bundle-onnx)")
	}
	if _, _, err := media.FindVips(""); err != nil {
		t.Skip("libvips not installed")
	}
	env := newTestEnv(t, func(c *auth.AppConfig) { c.SearchModel = model; c.SearchThreads = 4 })
	phone := env.enrolledClient(t)
	tablet := env.enrolledClient(t)
	if _, err := env.enrollment.Registry.SetWithholdPrivate(env.enrollment.Registry.List("hen")[1].Serial, true); err != nil {
		t.Fatal(err)
	}
	red := env.uploadPhoto(t, phone, plainJPEG(t, color.RGBA{220, 20, 20, 255}), "RED.jpg")
	blue := env.uploadPhoto(t, phone, plainJPEG(t, color.RGBA{20, 40, 220, 255}), "BLUE.jpg")

	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { env.search.Run(ctx); close(done) }()
	t.Cleanup(func() { stop(); <-done })
	for deadline := time.Now().Add(time.Minute); indexed(env) < 2; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the photos were not read in a minute")
		}
	}

	type result struct {
		Indexed, Total int
		Model          string
		Hits           []struct {
			Score       float64
			Set, Moment int
			Media       struct{ Sha256 string }
		}
	}
	search := func(client *http.Client, text string) result {
		t.Helper()
		resp := env.graphql(t, client, searchQuery, map[string]any{"t": text})
		var r result
		if err := json.Unmarshal(resp.Data["searchMedia"], &r); err != nil {
			t.Fatalf("searchMedia = %s %+v", resp.Data["searchMedia"], resp.Errors)
		}
		return r
	}
	got := search(phone, "a plain red picture")
	if got.Indexed != 2 || got.Total != 2 || got.Model != "siglip2-base-patch16-256" || len(got.Hits) != 2 {
		t.Fatalf("searchMedia = %+v", got)
	}
	if got.Hits[0].Media.Sha256 != red || got.Hits[0].Score <= got.Hits[1].Score {
		t.Errorf("red picture: hits %+v, want %s first", got.Hits, red)
	}
	// The next page: the second hit, numbered as on the first page.
	resp := env.graphql(t, phone, `query($t: String!) { searchMedia(text: $t, limit: 1, offset: 1) { hits { set moment media { sha256 } } } }`,
		map[string]any{"t": "a plain red picture"})
	var page result
	if err := json.Unmarshal(resp.Data["searchMedia"], &page); err != nil || len(page.Hits) != 1 || page.Hits[0].Media.Sha256 != got.Hits[1].Media.Sha256 || page.Hits[0].Set != 1 {
		t.Errorf("offset 1: %s %+v, want the second hit as set 1", resp.Data["searchMedia"], resp.Errors)
	}
	// Uploaded without a time, each is a set and a moment of its own, numbered by its best hit.
	if got.Hits[0].Set != 0 || got.Hits[1].Set != 1 || got.Hits[0].Moment != 0 || got.Hits[1].Moment != 1 {
		t.Errorf("sets and moments: %+v", got.Hits)
	}
	if got := search(phone, "A plain BLUE picture"); got.Hits[0].Media.Sha256 != blue {
		t.Errorf("blue picture: hits %+v, want %s first", got.Hits, blue)
	}

	// Years and an area are applied before the best are picked: the best red picture of 2019 in Paris is the dark
	// red one, though the bright red one, of 2026 in Tel Aviv, describes the text better.
	darkRed := env.uploadPhotoWith(t, phone, plainJPEG(t, color.RGBA{120, 10, 10, 255}), "DARK.jpg", map[string]any{
		"takenAt": "2019-06-01T12:00:00+02:00", "location": map[string]any{"lat": 48.86, "lon": 2.35},
	})
	for deadline := time.Now().Add(time.Minute); indexed(env) < 3; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the third photo was not read in a minute")
		}
	}
	filtered := func(vars map[string]any) result {
		t.Helper()
		vars["t"] = "a plain red picture"
		resp := env.graphql(t, phone, `query($t: String!, $y: [Int!], $a: GeoAreaInput) {
			searchMedia(text: $t, limit: 1, years: $y, area: $a) { indexed total model hits { score media { sha256 } } } }`, vars)
		var r result
		if err := json.Unmarshal(resp.Data["searchMedia"], &r); err != nil {
			t.Fatalf("searchMedia = %s %+v", resp.Data["searchMedia"], resp.Errors)
		}
		return r
	}
	if got := filtered(map[string]any{"y": []int{2019}}); len(got.Hits) != 1 || got.Hits[0].Media.Sha256 != darkRed {
		t.Errorf("2019: hits %+v, want the dark red picture", got.Hits)
	}
	paris := map[string]any{"south": 48.8, "west": 2.2, "north": 48.9, "east": 2.5}
	if got := filtered(map[string]any{"a": paris}); len(got.Hits) != 1 || got.Hits[0].Media.Sha256 != darkRed {
		t.Errorf("Paris: hits %+v, want the dark red picture", got.Hits)
	}
	if got := filtered(map[string]any{"y": []int{2020}, "a": paris}); len(got.Hits) != 0 {
		t.Errorf("2020 in Paris: hits %+v, want none", got.Hits)
	}

	// A device kept from private files neither finds nor counts them.
	env.graphql(t, phone, `mutation($h: [Sha256!]!) { setPrivate(sha256s: $h, private: true) { sha256 } }`,
		map[string]any{"h": []string{red}})
	// Private files are left out unless the app asks for them; they still count as read.
	if got := search(phone, "a plain red picture"); got.Total != 3 || got.Indexed != 3 || len(got.Hits) != 2 || got.Hits[0].Media.Sha256 != darkRed {
		t.Errorf("phone found %+v, want the dark red and blue pictures", got)
	}
	resp = env.graphql(t, phone, `query($t: String!) { searchMedia(text: $t, limit: 10, includePrivate: true) { hits { media { sha256 } } } }`,
		map[string]any{"t": "a plain red picture"})
	var withPrivate result
	if err := json.Unmarshal(resp.Data["searchMedia"], &withPrivate); err != nil || len(withPrivate.Hits) != 3 || withPrivate.Hits[0].Media.Sha256 != red {
		t.Errorf("includePrivate: %s %+v, want the red picture first", resp.Data["searchMedia"], resp.Errors)
	}
	if got := search(tablet, "a plain red picture"); got.Total != 2 || got.Indexed != 2 || len(got.Hits) != 2 || got.Hits[0].Media.Sha256 != darkRed {
		t.Errorf("tablet found %+v, want the dark red and blue pictures", got)
	}
}

// uploadPhotoWith uploads data like uploadPhoto, with more facts for startUpload (takenAt, location).
func (e *testEnv) uploadPhotoWith(t *testing.T, client *http.Client, data []byte, name string, facts map[string]any) string {
	t.Helper()
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	input := map[string]any{"sha256": sha, "size": len(data), "fileName": name, "mimeType": "image/jpeg", "kind": "PHOTO"}
	for k, v := range facts {
		input[k] = v
	}
	if start := e.graphql(t, client, startUploadMutation, map[string]any{"input": input}); !strings.Contains(string(start.Data["startUpload"]), "UploadReady") {
		t.Fatalf("startUpload = %s %+v", start.Data["startUpload"], start.Errors)
	}
	if resp := e.put(t, client, "/upload/"+sha, data, 0, len(data)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload %s: %d", name, resp.StatusCode)
	}
	return sha
}

// indexed is how many of hen's files search has read.
func indexed(env *testEnv) int {
	n, _ := env.search.Counts("hen", func(media.Item) bool { return false })
	return n
}

func plainJPEG(t *testing.T, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 320, 240))
	for y := range 240 {
		for x := range 320 {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
