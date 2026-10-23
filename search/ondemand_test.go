package search

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/media"
	"github.com/journal/server-go/onnx"
)

// The text half is loaded by the first search, released after IdleRelease without one, and loaded again by the
// next search; the vectors another process wrote are found all along.
func TestTheTextHalfIsLoadedOnlyWhileSearching(t *testing.T) {
	folder := filepath.Join("..", "models", "siglip2-base-patch16-256")
	library := filepath.Join("..", "onnxruntime", onnx.RuntimeLibrary())
	for _, need := range []string{filepath.Join(folder, "text_model.onnx"), library} {
		if _, err := os.Stat(need); err != nil {
			t.Skipf("%s is missing (go run ./scripts/bundle-onnx)", need)
		}
	}
	defer func(idle time.Duration) { IdleRelease = idle }(IdleRelease)
	IdleRelease = 300 * time.Millisecond

	root := t.TempDir()
	lib, err := media.OpenLibrary(map[string]string{"hen": root}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := onnx.LoadRuntime(library)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewOnDemandService(rt, folder, 4, lib)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.onDemand.loaded() {
		t.Fatal("loaded before any search")
	}
	// The indexer wrote a vector: the text's own, so the search must find it.
	m := loadTestModel(t)
	v, _ := m.TextVector("a red car")
	writer, err := OpenIndex(root, s.Model(), m.Dim)
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 64)
	writer.Add(sha, v)
	writer.Close()
	if err := s.Refresh(); err != nil {
		t.Fatal(err)
	}

	all := func(string) bool { return true }
	hits, err := s.Search("hen", "a red car", 1, all)
	if err != nil || len(hits) != 1 || hits[0].Sha256 != sha || !s.onDemand.loaded() {
		t.Fatalf("first search: %v, %v (loaded %v)", hits, err, s.onDemand.loaded())
	}
	time.Sleep(IdleRelease + 300*time.Millisecond)
	if s.onDemand.loaded() {
		t.Fatal("still loaded after the idle time")
	}
	if hits, err := s.Search("hen", "a red car", 1, all); err != nil || len(hits) != 1 || !s.onDemand.loaded() {
		t.Fatalf("search after release: %v, %v", hits, err)
	}

	// Prepared: loaded in the background, and a search sent meanwhile waits for the same load.
	time.Sleep(IdleRelease + 300*time.Millisecond)
	if s.State() != Unloaded {
		t.Fatalf("state after the idle time: %v", s.State())
	}
	if state := s.Prepare(); state != Loading {
		t.Fatalf("Prepare = %v", state)
	}
	if hits, err := s.Search("hen", "a red car", 1, all); err != nil || len(hits) != 1 || s.State() != Ready {
		t.Fatalf("search while loading: %v, %v", hits, err)
	}
	if state := s.Prepare(); state != Ready {
		t.Fatalf("Prepare once loaded = %v", state)
	}
}
