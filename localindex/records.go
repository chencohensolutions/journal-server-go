package localindex

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/journal/server-go/media"
)

// The headers of the binary formats, as package search and faces write them.
const (
	vectorsHeader = "journal-vectors 1\n"
	facesHeader   = "journal-faces 1\n"
)

// Numbers per face before its vector, in FormatFaces: box, score, points.
const faceFields = 4 + 1 + 10

// cacheFile is a cache file opened to read: its length when opened, the length of its vectors, and where its
// records begin.
type cacheFile struct {
	file   *os.File
	format string
	size   int64
	dim    int
	start  int64
}

// errNoHeader is a file without its whole header yet (being started, or not there).
var errNoHeader = errors.New("no header yet")

// openCache opens the cache file at path, of format, and reads its header.
func openCache(path, format string) (*cacheFile, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNoHeader
	}
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	c := &cacheFile{file: f, format: format, size: info.Size()}
	var header string
	switch format {
	case FormatVectors:
		header = vectorsHeader
	case FormatFaces:
		header = facesHeader
	case FormatJSONL:
		return c, nil
	default:
		f.Close()
		return nil, fmt.Errorf("no format %q", format)
	}
	head := make([]byte, len(header)+4)
	if _, err := io.ReadFull(f, head); err != nil {
		f.Close()
		return nil, errNoHeader
	}
	if string(head[:len(header)]) != header {
		f.Close()
		return nil, fmt.Errorf("%s is not a %s file", path, format)
	}
	c.dim = int(binary.LittleEndian.Uint32(head[len(header):]))
	c.start = int64(len(head))
	return c, nil
}

func (c *cacheFile) Close() error { return c.file.Close() }

// reader reads the records from offset at on, up to the length the file had when opened (what is appended later
// is for the next look).
func (c *cacheFile) reader(at int64) *recordReader {
	return &recordReader{c: c, r: bufio.NewReaderSize(io.NewSectionReader(c.file, at, c.size-at), 1<<20), at: at}
}

// recordReader reads one whole record at a time.
type recordReader struct {
	c  *cacheFile
	r  *bufio.Reader
	at int64 // where the next record begins
}

// next is the next whole record, the sha256 it is of (in hex; "" for a JSON line that isn't a record) and where it
// began; io.EOF at the end, or at a record not yet whole.
func (rr *recordReader) next() (record []byte, sha string, began int64, err error) {
	began = rr.at
	switch rr.c.format {
	case FormatVectors:
		record = make([]byte, 32+4*rr.c.dim)
		if _, err := io.ReadFull(rr.r, record); err != nil {
			return nil, "", began, io.EOF
		}
	case FormatFaces:
		head := make([]byte, 34)
		if _, err := io.ReadFull(rr.r, head); err != nil {
			return nil, "", began, io.EOF
		}
		n := int(binary.LittleEndian.Uint16(head[32:]))
		record = make([]byte, 34+n*4*(faceFields+rr.c.dim))
		copy(record, head)
		if _, err := io.ReadFull(rr.r, record[34:]); err != nil {
			return nil, "", began, io.EOF
		}
	case FormatJSONL:
		line, err := rr.r.ReadBytes('\n')
		if err != nil {
			return nil, "", began, io.EOF // the end, or a line still being written
		}
		rr.at += int64(len(line))
		var rec struct {
			Sha256 string `json:"sha256"`
		}
		line = line[:len(line)-1]
		if json.Unmarshal(line, &rec) != nil || !media.IsSha256(rec.Sha256) {
			return line, "", began, nil
		}
		return line, rec.Sha256, began, nil
	}
	rr.at += int64(len(record))
	return record, hex.EncodeToString(record[:32]), began, nil
}

// recordAt reports whether the record that begins at began is of sha and ends at end: whether a cursor's position
// is still in the same file (one started over since has other records there).
func (c *cacheFile) recordAt(began, end int64, sha string) bool {
	if began < c.start || end > c.size || began >= end {
		return false
	}
	record, got, _, err := c.reader(began).next()
	return err == nil && got == sha && began+int64(len(record))+c.newline() == end
}

// newline is the byte after each record that isn't part of it: a JSON line's newline.
func (c *cacheFile) newline() int64 {
	if c.format == FormatJSONL {
		return 1
	}
	return 0
}
