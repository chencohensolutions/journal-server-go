package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/media"
	"github.com/journal/server-go/search/searchtest"
)

func jpegOf(c color.RGBA) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 60, 40))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, 255
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, nil)
	return buf.Bytes()
}

const searchQuery = `query($q: String!) { searchMedia(query: $q) { indexed total hits { score media { sha256 } } } }`

type searchResult struct {
	Indexed, Total int
	Hits           []struct {
		Score float64
		Media struct{ Sha256 string }
	}
}

func TestSearchFindsUploadedPhotosByWhatTheyShow(t *testing.T) {
	if _, _, err := media.FindVips(""); err != nil {
		t.Skip("libvips not installed")
	}
	// This test binary is the model (TestMain), reading the previews libvips makes.
	t.Setenv(searchtest.Env, "1")
	env := newTestEnv(t, func(c *auth.AppConfig) { c.Search = &auth.SearchConfig{Python: os.Args[0], Model: "test/colours"} })
	phone := env.enrolledClient(t)
	tablet := env.enrolledClient(t)
	if _, err := env.enrollment.Registry.SetWithholdPrivate(env.enrollment.Registry.List("hen")[1].Serial, true); err != nil {
		t.Fatal(err)
	}
	red := env.uploadPhoto(t, phone, jpegOf(color.RGBA{R: 240, G: 20, B: 20}), "RED.jpg")
	blue := env.uploadPhoto(t, phone, jpegOf(color.RGBA{R: 20, G: 20, B: 240}), "BLUE.jpg")

	search := func(client *http.Client, query string) searchResult {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			resp := env.graphql(t, client, searchQuery, map[string]any{"q": query})
			var result searchResult
			json.Unmarshal(resp.Data["searchMedia"], &result)
			if len(resp.Errors) == 0 && result.Indexed == result.Total {
				return result
			}
			starting := len(resp.Errors) > 0 && strings.Contains(resp.Errors[0].Message, "starting")
			if len(resp.Errors) > 0 && !starting || time.Now().After(deadline) {
				t.Fatalf("searchMedia(%q) = %s %+v", query, resp.Data["searchMedia"], resp.Errors)
			}
		}
	}
	result := search(phone, "red")
	if result.Total != 2 || len(result.Hits) != 2 || result.Hits[0].Media.Sha256 != red || result.Hits[0].Score < 0.9 {
		t.Fatalf("phone searching red: %+v", result)
	}
	if result := search(phone, "blue"); result.Hits[0].Media.Sha256 != blue {
		t.Fatalf("phone searching blue: %+v", result)
	}

	// A device kept from private files neither finds nor counts them.
	env.graphql(t, phone, `mutation($h: [Sha256!]!) { setPrivate(sha256s: $h, private: true) { sha256 } }`,
		map[string]any{"h": []string{red}})
	if result := search(tablet, "red"); result.Total != 1 || len(result.Hits) != 1 || result.Hits[0].Media.Sha256 != blue {
		t.Fatalf("tablet searching red: %+v", result)
	}
	if result := search(phone, "red"); result.Total != 2 || result.Hits[0].Media.Sha256 != red {
		t.Fatalf("phone searching red after marking it private: %+v", result)
	}
}

func TestSearchIsOffUnlessConfigured(t *testing.T) {
	env := newTestEnv(t)
	resp := env.graphql(t, env.enrolledClient(t), searchQuery, map[string]any{"q": "red"})
	if len(resp.Errors) == 0 || !strings.Contains(resp.Errors[0].Message, "search is off") {
		t.Fatalf("searchMedia = %s %+v", resp.Data["searchMedia"], resp.Errors)
	}
}
