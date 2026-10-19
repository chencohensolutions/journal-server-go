package main

import (
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/journal/server-go/media"
)

// enrolledClient enrolls a device for hen and returns an HTTP client presenting its certificate.
func (e *testEnv) enrolledClient(t *testing.T) *http.Client {
	t.Helper()
	code, _ := e.enrollment.Codes.Create("hen")
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	result := e.enroll(t, code, key)
	certDER, _ := base64.StdEncoding.DecodeString(result.Certificate)
	return e.client(&tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: key})
}

func (e *testEnv) put(t *testing.T, client *http.Client, path string, data []byte, start, end int) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, e.server.URL+path, bytes.NewReader(data[start:end]))
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(data)))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

const startUploadMutation = `mutation($input: StartUploadInput!) {
	startUpload(input: $input) {
		__typename
		... on UploadReady { url offset chunkSize }
		... on MediaExists { media { sha256 } }
		... on UploadRejected { reason }
	}
}`

func TestDeviceUploadsAndStreamsAVideo(t *testing.T) {
	env := newTestEnv(t)
	device := env.enrolledClient(t)
	video := bytes.Repeat([]byte("0123456789"), 1000)
	sum := sha256.Sum256(video)
	sha := hex.EncodeToString(sum[:])

	missing := env.graphql(t, device, `query($h: [Sha256!]!) { missingMedia(hashes: $h) }`, map[string]any{"h": []string{sha}})
	if string(missing.Data["missingMedia"]) != `["`+sha+`"]` {
		t.Fatalf("missingMedia = %s %+v", missing.Data["missingMedia"], missing.Errors)
	}
	if bad := env.graphql(t, device, `{ missingMedia(hashes: ["ABC"]) }`, nil); len(bad.Errors) == 0 {
		t.Fatalf("a bad hash was accepted")
	}

	input := map[string]any{
		"sha256": sha, "size": len(video), "fileName": "VID_20261017_155222.mp4", "mimeType": "video/mp4",
		"kind": "VIDEO", "takenAt": "2026-10-17T15:52:22+03:00", "durationMs": 4200, "source": "camera",
		"location": map[string]any{"lat": 32.08, "lon": 34.78},
	}
	start := env.graphql(t, device, startUploadMutation, map[string]any{"input": input})
	var ready struct {
		Typename  string `json:"__typename"`
		URL       string
		Offset    int64
		ChunkSize int64
	}
	json.Unmarshal(start.Data["startUpload"], &ready)
	if ready.Typename != "UploadReady" || ready.URL != "/upload/"+sha || ready.Offset != 0 {
		t.Fatalf("startUpload = %s %+v", start.Data["startUpload"], start.Errors)
	}

	if resp := env.put(t, env.client(nil), ready.URL, video, 0, 4000); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous piece: %d", resp.StatusCode)
	}
	if resp := env.put(t, device, ready.URL, video, 0, 4000); resp.StatusCode != http.StatusOK || resp.Header.Get("Upload-Offset") != "4000" {
		t.Fatalf("first piece: %d %s", resp.StatusCode, resp.Header.Get("Upload-Offset"))
	}
	if resp := env.put(t, device, ready.URL, video, 2000, 6000); resp.StatusCode != http.StatusRequestedRangeNotSatisfiable || resp.Header.Get("Upload-Offset") != "4000" {
		t.Fatalf("overlapping piece: %d %s", resp.StatusCode, resp.Header.Get("Upload-Offset"))
	}
	head, _ := http.NewRequest(http.MethodHead, env.server.URL+ready.URL, nil)
	if resp, err := device.Do(head); err != nil || resp.Header.Get("Upload-Offset") != "4000" {
		t.Fatalf("HEAD: %v %v", resp, err)
	}
	if resp := env.put(t, device, ready.URL, video, 4000, len(video)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("last piece: %d", resp.StatusCode)
	}

	stored := filepath.Join(env.dataFolder, "media", "2026", "2026-10", "VID_20261017_155222.mp4")
	if data, err := os.ReadFile(stored); err != nil || !bytes.Equal(data, video) {
		t.Fatalf("stored file: %v", err)
	}

	media := env.graphql(t, device, `query($s: Sha256!) { media(sha256: $s) { kind size takenAt uploadedBy url location { lat } versions } }`, map[string]any{"s": sha})
	got := string(media.Data["media"])
	for _, want := range []string{`"kind":"VIDEO"`, `"size":10000`, `"takenAt":"2026-10-17T15:52:22+03:00"`, `"uploadedBy":"Pixel 9 Pro"`, `"url":"/media/` + sha + `"`, `"lat":32.08`, `"versions":[]`} {
		if !strings.Contains(got, want) {
			t.Errorf("media = %s, want %s (%+v)", got, want, media.Errors)
		}
	}
	again := env.graphql(t, device, startUploadMutation, map[string]any{"input": input})
	if !strings.Contains(string(again.Data["startUpload"]), "MediaExists") {
		t.Fatalf("second startUpload = %s", again.Data["startUpload"])
	}

	// The original streams with Range, which is how the players seek.
	req, _ := http.NewRequest(http.MethodGet, env.server.URL+"/media/"+sha, nil)
	req.Header.Set("Range", "bytes=10-19")
	resp, err := device.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(body) != "0123456789" || resp.Header.Get("Content-Type") != "video/mp4" || resp.Header.Get("ETag") != `"`+sha+`"` {
		t.Fatalf("range: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	anonymous, _ := env.client(nil).Get(env.server.URL + "/media/" + sha)
	anonymous.Body.Close()
	if anonymous.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous download: %d", anonymous.StatusCode)
	}
}

func TestPreviewOfAnUploadedPhoto(t *testing.T) {
	if _, _, err := media.FindVips(""); err != nil {
		t.Skip("libvips not installed")
	}
	env := newTestEnv(t)
	device := env.enrolledClient(t)
	img := image.NewRGBA(image.Rect(0, 0, 300, 200))
	var photo bytes.Buffer
	jpeg.Encode(&photo, img, nil)
	sum := sha256.Sum256(photo.Bytes())
	sha := hex.EncodeToString(sum[:])
	env.graphql(t, device, startUploadMutation, map[string]any{"input": map[string]any{
		"sha256": sha, "size": photo.Len(), "fileName": "IMG.jpg", "mimeType": "image/jpeg", "kind": "PHOTO",
	}})
	if resp := env.put(t, device, "/upload/"+sha, photo.Bytes(), 0, photo.Len()); resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d", resp.StatusCode)
	}

	resp, err := device.Get(env.server.URL + "/media/" + sha + "/preview?w=90&h=60")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/webp" {
		t.Fatalf("preview: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if bad, _ := device.Get(env.server.URL + "/media/" + sha + "/preview?w=0&h=60"); bad.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad size: %d", bad.StatusCode)
	}
}

func TestChangeFeedIsGzipped(t *testing.T) {
	env := newTestEnv(t)
	device := env.enrolledClient(t)
	photo := []byte("a photo")
	sum := sha256.Sum256(photo)
	sha := hex.EncodeToString(sum[:])
	env.graphql(t, device, startUploadMutation, map[string]any{"input": map[string]any{
		"sha256": sha, "size": len(photo), "fileName": "IMG.jpg", "mimeType": "image/jpeg", "kind": "PHOTO",
	}})
	env.put(t, device, "/upload/"+sha, photo, 0, len(photo))

	body, _ := json.Marshal(map[string]any{"query": `{ changes { cursor hasMore changes { kind path media { sha256 fileName } } } }`})
	req, _ := http.NewRequest(http.MethodPost, env.server.URL+"/query", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := device.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("not gzipped: %v", resp.Header)
	}
	unzipped, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(unzipped)
	if !strings.Contains(string(data), `"path":"media/`+sha+`"`) || !strings.Contains(string(data), `"hasMore":false`) {
		t.Fatalf("changes = %s", data)
	}
}
