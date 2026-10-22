package main

import (
	"bytes"
	"context"
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

const searchQuery = `query($t: String!) { searchMedia(text: $t, limit: 10) { indexed total model hits { score media { sha256 } } } }`

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
			Score float64
			Media struct{ Sha256 string }
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
	if got := search(phone, "A plain BLUE picture"); got.Hits[0].Media.Sha256 != blue {
		t.Errorf("blue picture: hits %+v, want %s first", got.Hits, blue)
	}

	// A device kept from private files neither finds nor counts them.
	env.graphql(t, phone, `mutation($h: [Sha256!]!) { setPrivate(sha256s: $h, private: true) { sha256 } }`,
		map[string]any{"h": []string{red}})
	if got := search(tablet, "a plain red picture"); got.Total != 1 || got.Indexed != 1 || len(got.Hits) != 1 || got.Hits[0].Media.Sha256 != blue {
		t.Errorf("tablet found %+v, want only the blue picture", got)
	}
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
