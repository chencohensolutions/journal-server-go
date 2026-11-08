package localindex

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type noCache struct{}

func (noCache) Model() string                  { return "m" }
func (noCache) CacheFile(user string) string   { return "" }
func (noCache) Has(user, sha string) bool      { return false }
func (noCache) Record(user, sha string) []byte { return nil }

// A model file is listed once hashed, and downloads in pieces with its sha256 as the ETag; only listed files are
// served.
func TestModelFilesDownloadWithRange(t *testing.T) {
	folder := t.TempDir()
	data := []byte("the text half of a model, made up")
	os.WriteFile(filepath.Join(folder, "text_model.onnx"), data, 0o644)
	os.WriteFile(filepath.Join(folder, "secret.txt"), []byte("not for phones"), 0o644)
	s := New(nil, nil, Source{Name: "search", Format: FormatVectors, Cache: noCache{}, ModelFolder: folder,
		ModelFiles: []ModelFileSpec{{Name: "text_model.onnx", Precision: "fp32"}, {Name: "text_model_q8.onnx", Precision: "q8"}}})
	var files []ModelFile
	for deadline := time.Now().Add(5 * time.Second); len(files) == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		files = s.models.of("search")
	}
	sum := sha256.Sum256(data)
	if len(files) != 1 || files[0].Sha256 != hex.EncodeToString(sum[:]) || files[0].Bytes != int64(len(data)) ||
		files[0].URL != "/models/search/text_model.onnx" {
		t.Fatalf("listed: %+v, want only the file that is there", files)
	}
	if _, err := os.Stat(filepath.Join(folder, hashesFile)); err != nil {
		t.Errorf("the hash isn't kept: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /models/{index}/{name}", s.ServeModel)
	server := httptest.NewServer(mux)
	defer server.Close()
	req, _ := http.NewRequest(http.MethodGet, server.URL+files[0].URL, nil)
	req.Header.Set("Range", "bytes=4-7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	piece, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(piece) != "text" || resp.Header.Get("ETag") != `"`+files[0].Sha256+`"` {
		t.Fatalf("a piece: %d %q %s", resp.StatusCode, piece, resp.Header.Get("ETag"))
	}
	for _, p := range []string{"/models/search/secret.txt", "/models/search/text_model_q8.onnx", "/models/search/..%2Fsecret.txt", "/models/faces/text_model.onnx"} {
		resp, err := http.Get(server.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
}

// The models phones run by themselves are offered under /models/phone/ once hashed, even with no index to keep.
func TestPhoneModelsAreOfferedWithoutAnIndex(t *testing.T) {
	folder := t.TempDir()
	data := []byte("a photo embedder, made up")
	os.WriteFile(filepath.Join(folder, "embedder.tflite"), data, 0o644)
	os.WriteFile(filepath.Join(folder, "other.tflite"), []byte("another"), 0o644)
	sum := sha256.Sum256(data)
	// Where each came from, as bundle-onnx writes it: other.tflite's sha256 is no longer the one written there.
	os.WriteFile(filepath.Join(folder, sourcesFile), []byte(`{
		"embedder.tflite": {"url": "https://storage.example/embedder.tflite", "sha256": "`+hex.EncodeToString(sum[:])+`"},
		"other.tflite": {"url": "https://storage.example/other.tflite", "sha256": "0000"}}`), 0o644)
	s := NewWithPhoneModels(nil, nil, folder, []ModelFileSpec{{Name: "embedder.tflite"}, {Name: "other.tflite"}, {Name: "missing.tflite"}})
	if s.On() {
		t.Error("a server with no index says phones may keep one")
	}
	var files []ModelFile
	for deadline := time.Now().Add(5 * time.Second); len(files) < 2 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		files = s.PhoneModels()
	}
	if len(files) != 2 || files[0].URL != "/models/phone/embedder.tflite" || files[0].Sha256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("listed: %+v, want only the files that are there", files)
	}
	if files[0].Source != "https://storage.example/embedder.tflite" || files[1].Source != "" {
		t.Errorf("sources: %q and %q, want only the one whose sha256 still matches", files[0].Source, files[1].Source)
	}
	if got := (*Server)(nil).PhoneModels(); got != nil {
		t.Errorf("a nil server: %+v", got)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /models/{index}/{name}", s.ServeModel)
	server := httptest.NewServer(mux)
	defer server.Close()
	resp, err := http.Get(server.URL + files[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != string(data) {
		t.Fatalf("download: %d %q", resp.StatusCode, body)
	}
}
