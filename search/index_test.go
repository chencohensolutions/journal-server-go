package search

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sha(c byte) string { return strings.Repeat(string(c), 64) }

func TestIndexKeepsVectorsAcrossRestarts(t *testing.T) {
	root := t.TempDir()
	x, err := OpenIndex(root, "m", 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, add := range []struct {
		sha    string
		vector []float32
	}{{sha('a'), []float32{1, 0}}, {sha('b'), []float32{0, 1}}, {sha('c'), []float32{0.6, 0.8}}} {
		if err := x.Add(add.sha, add.vector); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.Add("not a sha", []float32{1, 0}); err == nil {
		t.Error("added a vector under a bad sha256")
	}
	if err := x.Add(sha('d'), []float32{1, 0, 0}); err == nil {
		t.Error("added a vector of the wrong length")
	}
	x.Close()

	// The server stopped halfway through writing a record: it is dropped, the others are kept.
	path := filepath.Join(root, ".cache", "search", "m.vectors")
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	f.Write([]byte{1, 2, 3})
	f.Close()

	x, err = OpenIndex(root, "m", 2)
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	if x.Len() != 3 || !x.Has(sha('b')) || x.Has(sha('d')) {
		t.Fatalf("reopened with %d vectors", x.Len())
	}
	hits := x.Search([]float32{1, 0}, 2, func(string) bool { return true })
	if len(hits) != 2 || hits[0].Sha256 != sha('a') || hits[1].Sha256 != sha('c') || hits[1].Score != 0.6 {
		t.Fatalf("hits %+v", hits)
	}
	hits = x.Search([]float32{1, 0}, 2, func(s string) bool { return s != sha('a') })
	if len(hits) != 2 || hits[0].Sha256 != sha('c') || hits[1].Sha256 != sha('b') {
		t.Fatalf("hits without a: %+v", hits)
	}

	// New records go after the dropped piece's place, so the file opens whole again.
	if err := x.Add(sha('d'), []float32{0, -1}); err != nil {
		t.Fatal(err)
	}
	x.Close()
	if x, err = OpenIndex(root, "m", 2); err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	if x.Len() != 4 {
		t.Fatalf("reopened with %d vectors", x.Len())
	}
}

func TestIndexStartsOverForAnotherVectorLength(t *testing.T) {
	root := t.TempDir()
	x, _ := OpenIndex(root, "m", 2)
	x.Add(sha('a'), []float32{1, 0})
	x.Close()
	x, err := OpenIndex(root, "m", 3)
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	if x.Len() != 0 {
		t.Fatalf("%d vectors of the old length kept", x.Len())
	}
	if err := x.Add(sha('a'), []float32{1, 0, 0}); err != nil {
		t.Fatal(err)
	}
}
