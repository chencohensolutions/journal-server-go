package search

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// vectors is one model's vectors of one user's files, in memory and in .cache/search/<model>.vectors. The file is a
// cache: deleting it reads every file again. Only the server writes it, so it takes no turn (media/turns.go).
//
// The file is a header line, "journal-vectors\t1\t<model>\t<dims>\n", then one record per file read, appended as
// files are read: the sha256 (32 bytes), a scale (float32), and the vector as dims int8 that the scale turns back
// into the unit vector, near enough to rank by. A record cut short by a crash is dropped when the file is opened.
type vectors struct {
	mu   sync.RWMutex
	file *os.File
	dims int
	rows map[string]int // by sha256
	// Row i is scales[i] × data[i*dims : (i+1)*dims].
	scales []float32
	data   []int8
}

const vectorsFormat = 1

func vectorsPath(root, model string) string {
	return filepath.Join(root, ".cache", "search", fileSafe(model)+".vectors")
}

// fileSafe turns a model's name ("google/siglip2-base-patch16-384") into a file name.
func fileSafe(model string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, model)
}

// openVectors reads the file of model in root, or starts it. A file written for another vector size is started
// again.
func openVectors(root, model string, dims int) (*vectors, error) {
	path := vectorsPath(root, model)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	v := &vectors{file: file, dims: dims, rows: map[string]int{}}
	header := fmt.Sprintf("journal-vectors\t%d\t%s\t%d\n", vectorsFormat, model, dims)
	data, err := io.ReadAll(file)
	if err != nil {
		file.Close()
		return nil, err
	}
	if !bytes.HasPrefix(data, []byte(header)) {
		if len(data) > 0 {
			log.Printf("search: %s is of another model or format; reading every file again", path)
		}
		data = nil
		if err := v.restart(header); err != nil {
			file.Close()
			return nil, err
		}
	} else {
		data = data[len(header):]
	}
	size := v.recordSize()
	whole := len(data) / size * size
	if whole != len(data) {
		if err := file.Truncate(int64(len(header) + whole)); err != nil {
			file.Close()
			return nil, err
		}
	}
	for record := range pieces(data[:whole], size) {
		v.load(record)
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		file.Close()
		return nil, err
	}
	return v, nil
}

func (v *vectors) restart(header string) error {
	if err := v.file.Truncate(0); err != nil {
		return err
	}
	_, err := v.file.WriteAt([]byte(header), 0)
	return err
}

func (v *vectors) recordSize() int { return 32 + 4 + v.dims }

// pieces yields data in pieces of size.
func pieces(data []byte, size int) func(func([]byte) bool) {
	return func(yield func([]byte) bool) {
		for i := 0; i+size <= len(data); i += size {
			if !yield(data[i : i+size]) {
				return
			}
		}
	}
}

// load indexes one record; a later record of the same file replaces the earlier one.
func (v *vectors) load(record []byte) {
	sha := hex.EncodeToString(record[:32])
	scale := math.Float32frombits(binary.LittleEndian.Uint32(record[32:]))
	values := make([]int8, v.dims)
	for i, b := range record[36:] {
		values[i] = int8(b)
	}
	if row, ok := v.rows[sha]; ok {
		v.scales[row] = scale
		copy(v.data[row*v.dims:], values)
		return
	}
	v.rows[sha] = len(v.scales)
	v.scales = append(v.scales, scale)
	v.data = append(v.data, values...)
}

// add keeps the vectors of files just read, in memory and at the end of the file.
func (v *vectors) add(shas []string, vecs [][]float32) error {
	var buf bytes.Buffer
	records := make([][]byte, len(shas))
	for i, sha := range shas {
		raw, err := hex.DecodeString(sha)
		if err != nil || len(raw) != 32 || len(vecs[i]) != v.dims {
			return fmt.Errorf("bad vector for %q", sha)
		}
		record := make([]byte, 36, v.recordSize())
		copy(record, raw)
		scale, values := quantize(vecs[i])
		binary.LittleEndian.PutUint32(record[32:], math.Float32bits(scale))
		for _, value := range values {
			record = append(record, byte(value))
		}
		records[i] = record
		buf.Write(record)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, err := v.file.Write(buf.Bytes()); err != nil {
		return err
	}
	for _, record := range records {
		v.load(record)
	}
	return nil
}

// quantize turns a vector into int8 and the scale that turns them back.
func quantize(vec []float32) (scale float32, values []int8) {
	var largest float32
	for _, x := range vec {
		largest = max(largest, float32(math.Abs(float64(x))))
	}
	values = make([]int8, len(vec))
	if largest == 0 {
		return 0, values
	}
	scale = largest / 127
	for i, x := range vec {
		values[i] = int8(math.Round(float64(x / scale)))
	}
	return scale, values
}

func (v *vectors) has(sha string) bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	_, ok := v.rows[sha]
	return ok
}

// scores are how close each file in shas is to query (a unit vector), -1 to 1; a file without a vector gets NaN.
func (v *vectors) scores(query []float32, shas []string) []float32 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	scores := make([]float32, len(shas))
	parts := max(1, runtime.NumCPU())
	chunk := (len(shas) + parts - 1) / parts
	var wg sync.WaitGroup
	for start := 0; start < len(shas); start += chunk {
		wg.Go(func() {
			for i := start; i < min(start+chunk, len(shas)); i++ {
				row, ok := v.rows[shas[i]]
				if !ok {
					scores[i] = float32(math.NaN())
					continue
				}
				var sum float32
				for j, x := range v.data[row*v.dims : (row+1)*v.dims] {
					sum += query[j] * float32(x)
				}
				scores[i] = sum * v.scales[row]
			}
		})
	}
	wg.Wait()
	return scores
}

func (v *vectors) close() error { return v.file.Close() }
