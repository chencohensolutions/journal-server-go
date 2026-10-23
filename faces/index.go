package faces

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync"
)

// Index is the faces found in one user's files by one model, kept in .cache/faces/<model>.faces in the data
// folder. It is rebuilt when lost: the faces come from the files.
//
// The file is a header ("journal-faces 1\n" and the vector length as a 4-byte little-endian number) and then a
// record per file, appended as files are read: the 32 bytes of its sha256, how many faces it has (2 bytes; 0 for
// a file read that has none), and for each face its box (4 numbers), score (1), five points (10) and vector, all
// as 4-byte little-endian floats. A record cut short (the server stopped while writing it) is dropped when the
// file is opened. Only one process writes it: the server, or `journal-server index` (config.json "index":
// "daily"), while the server reads it (OpenIndexReadOnly, Refresh).
type Index struct {
	path string
	// Opened to read only: never written or cut, and read on from end by Refresh.
	readOnly bool
	end      int64

	mu    sync.RWMutex
	file  *os.File
	files map[string][]Face // by sha256
	faces int
}

const indexHeader = "journal-faces 1\n"

// Numbers per face before its vector: box, score, points.
const faceFields = 4 + 1 + 10

// OpenIndex opens (or starts) the faces of model in the data folder root.
func OpenIndex(root, model string) (*Index, error) {
	path := filepath.Join(root, ".cache", "faces", model+".faces")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	x := &Index{path: path, files: map[string][]Face{}}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	end, err := x.read(f)
	if err != nil {
		log.Printf("faces: starting %s over: %v", path, err)
		x.files, x.faces = map[string][]Face{}, 0
		end = 0
	}
	if end == 0 {
		var header bytes.Buffer
		header.WriteString(indexHeader)
		binary.Write(&header, binary.LittleEndian, uint32(Dim))
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
		return 0, errors.New("not a faces file")
	}
	if dim := int(binary.LittleEndian.Uint32(header[len(indexHeader):])); dim != Dim {
		return 0, fmt.Errorf("vectors of %d numbers, the model makes %d", dim, Dim)
	}
	end, _ := x.records(r, int64(len(header)))
	return end, nil
}

// records reads whole records from r, which is at end, and returns where the last whole one ends and how many
// there were: the end of r, or a record cut short (being written, or by a stop), ends them.
func (x *Index) records(r io.Reader, end int64) (int64, int) {
	head := make([]byte, 34)
	face := make([]byte, 4*(faceFields+Dim))
	n := 0
	for {
		if _, err := io.ReadFull(r, head); err != nil {
			return end, n
		}
		faces := make([]Face, binary.LittleEndian.Uint16(head[32:]))
		for i := range faces {
			if _, err := io.ReadFull(r, face); err != nil {
				return end, n
			}
			faces[i] = decodeFace(face)
		}
		x.put(hex.EncodeToString(head[:32]), faces)
		end += int64(len(head) + len(faces)*len(face))
		n++
	}
}

// OpenIndexReadOnly opens the faces of model in the data folder root to read only, for a server whose indexes are
// written by another process (journal-server index): nothing is ever written or cut, and Refresh reads on what
// the other process appends. A file not there yet is an empty index.
func OpenIndexReadOnly(root, model string) (*Index, error) {
	x := &Index{path: filepath.Join(root, ".cache", "faces", model+".faces"), files: map[string][]Face{}, readOnly: true}
	if _, err := x.Refresh(); err != nil {
		return nil, err
	}
	return x, nil
}

// Refresh reads the records appended since the last look, by the process that writes the file, and returns how
// many there were; a record still being written is left for the next look. A file started over is read again
// from its start. Only for an index opened read-only.
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
		x.files, x.faces, x.end = map[string][]Face{}, 0, 0
	}
	if x.end == 0 {
		header := make([]byte, len(indexHeader)+4)
		if _, err := io.ReadFull(f, header); err != nil {
			return 0, nil // not even its header yet
		}
		if string(header[:len(indexHeader)]) != indexHeader {
			return 0, fmt.Errorf("%s is not a faces file", x.path)
		}
		if dim := int(binary.LittleEndian.Uint32(header[len(indexHeader):])); dim != Dim {
			return 0, fmt.Errorf("%s has vectors of %d numbers, the model makes %d", x.path, dim, Dim)
		}
		x.end = int64(len(header))
	}
	if _, err := f.Seek(x.end, io.SeekStart); err != nil {
		return 0, err
	}
	var n int
	x.end, n = x.records(bufio.NewReaderSize(f, 1<<20), x.end)
	return n, nil
}

// put keeps faces in memory; a sha256 that comes again has its faces replaced.
func (x *Index) put(sha string, faces []Face) {
	x.faces += len(faces) - len(x.files[sha])
	x.files[sha] = faces
}

// Has reports whether sha has been read.
func (x *Index) Has(sha string) bool {
	x.mu.RLock()
	defer x.mu.RUnlock()
	_, ok := x.files[sha]
	return ok
}

// Faces is the faces found in sha, biggest first; none when it hasn't been read or has none. They must not be
// changed.
func (x *Index) Faces(sha string) []Face {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.files[sha]
}

// Len is how many files have been read, and how many faces were found in them.
func (x *Index) Len() (files, faces int) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return len(x.files), x.faces
}

// Range calls fn with each file read and its faces, in no order. fn must not call the index.
func (x *Index) Range(fn func(sha string, faces []Face)) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	for sha, faces := range x.files {
		fn(sha, faces)
	}
}

// Add saves the faces found in the file sha (none is saved too: the file has been read).
func (x *Index) Add(sha string, faces []Face) error {
	if x.readOnly {
		return errors.New("the index is read from another process's file, not written here")
	}
	raw, err := hex.DecodeString(sha)
	if err != nil || len(raw) != 32 {
		return fmt.Errorf("%q is not a sha256", sha)
	}
	if len(faces) > math.MaxUint16 {
		faces = faces[:math.MaxUint16]
	}
	record := make([]byte, 34, 34+len(faces)*4*(faceFields+Dim))
	copy(record, raw)
	binary.LittleEndian.PutUint16(record[32:], uint16(len(faces)))
	for _, f := range faces {
		if len(f.Vector) != Dim {
			return fmt.Errorf("a vector of %d numbers, the index keeps %d", len(f.Vector), Dim)
		}
		record = encodeFace(record, f)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if _, err := x.file.Write(record); err != nil {
		return err
	}
	x.put(sha, faces)
	return nil
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

func encodeFace(b []byte, f Face) []byte {
	put := func(v float32) { b = binary.LittleEndian.AppendUint32(b, math.Float32bits(v)) }
	for _, v := range f.Box {
		put(v)
	}
	put(f.Score)
	for _, v := range f.Landmarks {
		put(v)
	}
	for _, v := range f.Vector {
		put(v)
	}
	return b
}

func decodeFace(b []byte) Face {
	next := func() float32 {
		v := math.Float32frombits(binary.LittleEndian.Uint32(b))
		b = b[4:]
		return v
	}
	var f Face
	for i := range f.Box {
		f.Box[i] = next()
	}
	f.Score = next()
	for i := range f.Landmarks {
		f.Landmarks[i] = next()
	}
	f.Vector = make([]float32, Dim)
	for i := range f.Vector {
		f.Vector[i] = next()
	}
	return f
}
