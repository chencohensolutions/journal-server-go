package search

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// Index is the vectors of one user's files for one model, kept in .cache/search/<model>.vectors in the data
// folder. It is rebuilt when lost: the vectors come from the files.
//
// The file is a header ("journal-vectors 1\n" and the vector length as a 4-byte little-endian number) and then a
// record per file, appended as files are read: the 32 bytes of its sha256, then its vector as 4-byte little-endian
// floats. A record cut short (the server stopped while writing it) is dropped when the file is opened. Only one
// process writes it: the server, or `journal-server index` (config.json "index": "daily"), while the server reads
// it (OpenVectorsReadOnly, Refresh).
type Index struct {
	path string
	dim  int
	// Opened to read only: never written or cut, and read on from end by Refresh.
	readOnly bool
	end      int64

	mu      sync.RWMutex
	file    *os.File
	shas    []string
	vectors []float32 // shas[i]'s vector is vectors[i*dim : (i+1)*dim]
	row     map[string]int
}

const indexHeader = "journal-vectors 1\n"

// OpenIndex opens (or starts) the vectors of model in the data folder root. A file of vectors of another length
// is started over.
func OpenIndex(root, model string, dim int) (*Index, error) {
	return OpenVectors(filepath.Join(root, ".cache", "search", model+".vectors"), dim)
}

// OpenVectors opens (or starts) the vectors file at path, of vectors of dim numbers: the format of OpenIndex, for
// the other indexes that keep a vector per file (package similar, aesthetic).
func OpenVectors(path string, dim int) (*Index, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	x := &Index{path: path, dim: dim, row: map[string]int{}}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	end, err := x.read(f)
	if err != nil {
		log.Printf("search: starting %s over: %v", path, err)
		x.shas, x.vectors, x.row = nil, nil, map[string]int{}
		end = 0
	}
	if end == 0 {
		var header bytes.Buffer
		header.WriteString(indexHeader)
		binary.Write(&header, binary.LittleEndian, uint32(dim))
		if _, err := f.WriteAt(header.Bytes(), 0); err != nil {
			f.Close()
			return nil, err
		}
		end = int64(header.Len())
	}
	// Drop a record cut short, and anything after a header that was started over.
	if err := f.Truncate(end); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(end, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	x.file = f
	return x, nil
}

// OpenVectorsReadOnly opens the vectors file at path to read only, for a server whose indexes are written by
// another process (journal-server index): nothing is ever written or cut, and Refresh reads on what the other
// process appends. dim 0 takes the vectors' length from the file. A file not there yet is an empty index.
func OpenVectorsReadOnly(path string, dim int) (*Index, error) {
	x := &Index{path: path, dim: dim, row: map[string]int{}, readOnly: true}
	if _, err := x.Refresh(); err != nil {
		return nil, err
	}
	return x, nil
}

// Refresh reads the records appended since the last look, by the process that writes the file, and returns how
// many there were; a record still being written is left for the next look. A file started over (another model's
// length, or deleted) is read again from its start. Only for an index opened read-only.
func (x *Index) Refresh() (int, error) {
	if !x.readOnly {
		return 0, errors.New("the index is written here, not read from another process")
	}
	f, err := os.Open(x.path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if info.Size() < x.end {
		x.shas, x.vectors, x.row, x.end = nil, nil, map[string]int{}, 0
	}
	if x.end == 0 {
		header := make([]byte, len(indexHeader)+4)
		if _, err := io.ReadFull(f, header); err != nil {
			return 0, nil // not even its header yet
		}
		if string(header[:len(indexHeader)]) != indexHeader {
			return 0, fmt.Errorf("%s is not a vectors file", x.path)
		}
		dim := int(binary.LittleEndian.Uint32(header[len(indexHeader):]))
		if x.dim != 0 && dim != x.dim {
			return 0, fmt.Errorf("%s has vectors of %d numbers, the model makes %d", x.path, dim, x.dim)
		}
		x.dim, x.end = dim, int64(len(header))
	}
	if _, err := f.Seek(x.end, io.SeekStart); err != nil {
		return 0, err
	}
	// Room for every whole record in the file at once: grown record by record, a library's vectors (hundreds of
	// MB) would leave copies behind for the garbage collector to give back later.
	if more := int(info.Size()-x.end) / x.recordSize(); more > 0 {
		x.shas = slices.Grow(x.shas, more)
		x.vectors = slices.Grow(x.vectors, more*x.dim)
	}
	r := bufio.NewReaderSize(f, 1<<20)
	record := make([]byte, x.recordSize())
	n := 0
	for {
		if _, err := io.ReadFull(r, record); err != nil {
			return n, nil // the end, or a record still being written
		}
		x.put(hex.EncodeToString(record[:32]), decodeFloats(record[32:]))
		x.end += int64(len(record))
		n++
	}
}

// Dim is how many numbers a vector has; 0 for an index read from a file that has no header yet.
func (x *Index) Dim() int {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.dim
}

// read loads the records of f and returns where the last whole one ends; 0 when f is empty.
func (x *Index) read(f *os.File) (int64, error) {
	r := bufio.NewReaderSize(f, 1<<20)
	header := make([]byte, len(indexHeader)+4)
	if _, err := io.ReadFull(r, header); err != nil {
		if err == io.EOF {
			return 0, nil
		}
		return 0, errors.New("no header")
	}
	if string(header[:len(indexHeader)]) != indexHeader {
		return 0, errors.New("not a vectors file")
	}
	if dim := int(binary.LittleEndian.Uint32(header[len(indexHeader):])); dim != x.dim {
		return 0, fmt.Errorf("vectors of %d numbers, the model makes %d", dim, x.dim)
	}
	end := int64(len(header))
	record := make([]byte, x.recordSize())
	for {
		if _, err := io.ReadFull(r, record); err != nil {
			return end, nil // the end, or a record cut short
		}
		x.put(hex.EncodeToString(record[:32]), decodeFloats(record[32:]))
		end += int64(len(record))
	}
}

func (x *Index) recordSize() int { return 32 + 4*x.dim }

// put keeps vector in memory; a sha256 that comes again has its vector replaced.
func (x *Index) put(sha string, vector []float32) {
	if i, ok := x.row[sha]; ok {
		copy(x.vectors[i*x.dim:], vector)
		return
	}
	x.row[sha] = len(x.shas)
	x.shas = append(x.shas, sha)
	x.vectors = append(x.vectors, vector...)
}

// Has reports whether sha has a vector.
func (x *Index) Has(sha string) bool {
	x.mu.RLock()
	defer x.mu.RUnlock()
	_, ok := x.row[sha]
	return ok
}

// Vector is a copy of sha's vector (of length 1), or nil when it has none.
func (x *Index) Vector(sha string) []float32 {
	x.mu.RLock()
	defer x.mu.RUnlock()
	i, ok := x.row[sha]
	if !ok {
		return nil
	}
	return slices.Clone(x.vectors[i*x.dim : (i+1)*x.dim])
}

// Path is the vectors file.
func (x *Index) Path() string { return x.path }

// Record is sha's record as the file keeps it (its sha256's 32 bytes, then its vector), from memory; nil when it has
// no vector. Phones that keep the index get records this way (package localindex).
func (x *Index) Record(sha string) []byte {
	x.mu.RLock()
	defer x.mu.RUnlock()
	i, ok := x.row[sha]
	if !ok {
		return nil
	}
	raw, _ := hex.DecodeString(sha)
	record := make([]byte, 32, x.recordSize())
	copy(record, raw)
	for _, v := range x.vectors[i*x.dim : (i+1)*x.dim] {
		record = binary.LittleEndian.AppendUint32(record, math.Float32bits(v))
	}
	return record
}

// Len is how many files have a vector.
func (x *Index) Len() int {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return len(x.shas)
}

// Add saves the vector of the file sha (of length 1, from the index's model).
func (x *Index) Add(sha string, vector []float32) error {
	if x.readOnly {
		return errors.New("the index is read from another process's file, not written here")
	}
	if len(vector) != x.dim {
		return fmt.Errorf("a vector of %d numbers, the index keeps %d", len(vector), x.dim)
	}
	raw, err := hex.DecodeString(sha)
	if err != nil || len(raw) != 32 {
		return fmt.Errorf("%q is not a sha256", sha)
	}
	record := make([]byte, 32, x.recordSize())
	copy(record, raw)
	for _, v := range vector {
		record = binary.LittleEndian.AppendUint32(record, math.Float32bits(v))
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if _, err := x.file.Write(record); err != nil {
		return err
	}
	x.put(sha, vector)
	return nil
}

// View calls fn with every vector (shas[i]'s is vectors[i*dim : (i+1)*dim]), in the order they were added, while
// the index can't change. fn must not keep or change them, nor call the index.
func (x *Index) View(fn func(shas []string, vectors []float32, dim int)) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	fn(x.shas, x.vectors, x.dim)
}

// Close closes the file; the index can't be added to after.
func (x *Index) Close() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.file == nil {
		return nil
	}
	return x.file.Close()
}

// Hit is a file a search found, with how close its vector is to the text's: the cosine similarity, -1 to 1.
// SigLIP's are small: a good match is often 0.1 to 0.2.
type Hit struct {
	Sha256 string
	Score  float32
}

// Search returns the limit files closest to query (a vector of length 1), best first, of those keep accepts.
func (x *Index) Search(query []float32, limit int, keep func(sha string) bool) []Hit {
	if len(query) != x.dim || limit < 1 {
		return nil
	}
	x.mu.RLock()
	hits := make([]Hit, 0, len(x.shas))
	for i, sha := range x.shas {
		var score float32
		for j, v := range x.vectors[i*x.dim : (i+1)*x.dim] {
			score += v * query[j]
		}
		hits = append(hits, Hit{sha, score})
	}
	x.mu.RUnlock()
	slices.SortFunc(hits, func(a, b Hit) int { return cmp.Compare(b.Score, a.Score) })
	kept := hits[:0]
	for _, h := range hits {
		if len(kept) == limit {
			break
		}
		if keep(h.Sha256) {
			kept = append(kept, h)
		}
	}
	return kept
}

func decodeFloats(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}
