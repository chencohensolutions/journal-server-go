// Package localindex gives phones the indexes to keep and search themselves: each index's cache file (the
// vectors of text search, similar photos and aesthetic scores, the faces, the text in photos) as a stream of the
// records added since the phone's last look, the people and groups of faces whole, and the model files a phone needs
// to run an index (SigLIP's text half and tokenizer, to read a search's words). Only what the device may see is sent
// (media.View). See docs/search.md "On the phone".
package localindex

import (
	"github.com/journal/server-go/media"
)

// The formats of the cache files, as the indexes write them.
const (
	// FormatVectors: "journal-vectors 1\n", the vector length as 4 bytes, then per file its sha256's 32 bytes and
	// its vector as 4-byte floats (package search, similar, aesthetic). All little-endian.
	FormatVectors = "vectors"
	// FormatFaces: "journal-faces 1\n", the vector length as 4 bytes, then per file its sha256's 32 bytes, how many
	// faces (2 bytes), and per face box (4), score (1), five points (10) and vector as 4-byte floats (package faces).
	FormatFaces = "faces"
	// FormatJSONL: a JSON line per file, {"sha256", "lines": [{"text", "box", "score"}]} (package ocr).
	FormatJSONL = "jsonl"
)

// Cache is an index whose cache file phones can keep.
type Cache interface {
	// Model is the name of the index's model; the phone starts over when it changes.
	Model() string
	// CacheFile is user's cache file.
	CacheFile(user string) string
	// Has reports whether user's file sha has been read.
	Has(user, sha string) bool
	// Record is user's file sha's record as the cache file keeps it (a JSON line without its newline), from memory;
	// nil when it hasn't been read.
	Record(user, sha string) []byte
}

// Source is one index as phones get it.
type Source struct {
	// Name is the index's name in the URL and the manifest: search, faces, ocr, similar, aesthetic.
	Name   string
	Format string
	Cache  Cache
	// ModelFolder is the folder of the model files a phone needs to run the index; "" when it needs none.
	ModelFolder string
	// ModelFiles are those files, by name in ModelFolder, and the precision each is ("" for one any precision uses).
	ModelFiles []ModelFileSpec
}

// ModelFileSpec is a model file a phone may download.
type ModelFileSpec struct {
	Name      string
	Precision string
}

// Server answers the phones' requests for the indexes.
type Server struct {
	library *media.Library
	sources map[string]*Source
	order   []string
	models  *modelFiles
	// People is the face index's people and groups, nil when face search is off.
	people Members
}

// PhoneIndex is the name the models phones run by themselves are served under: /models/phone/{name}.
const PhoneIndex = "phone"

// New is a server of sources and, when people isn't nil, the people of the face index. It hashes the model files in
// the background (Manifest lists each once it is hashed).
func New(library *media.Library, people Members, sources ...Source) *Server {
	return NewWithPhoneModels(library, people, "", nil, sources...)
}

// NewWithPhoneModels is New that also offers phone, files in phoneFolder that phones run by themselves, not as part
// of an index the server keeps (the photo embedder the Android app finds near-duplicates with). PhoneModels lists
// them once hashed.
func NewWithPhoneModels(library *media.Library, people Members, phoneFolder string, phone []ModelFileSpec, sources ...Source) *Server {
	s := &Server{library: library, sources: map[string]*Source{}, people: people, models: newModelFiles()}
	for _, f := range phone {
		s.models.add(PhoneIndex, phoneFolder, f)
	}
	for _, src := range sources {
		s.sources[src.Name] = &src
		s.order = append(s.order, src.Name)
		for _, f := range src.ModelFiles {
			s.models.add(src.Name, src.ModelFolder, f)
		}
	}
	go s.models.hashAll()
	return s
}

// PhoneModels is the model files phones run by themselves whose sha256 is known; none on a nil server.
func (s *Server) PhoneModels() []ModelFile {
	if s == nil {
		return nil
	}
	return s.models.of(PhoneIndex)
}

// On reports whether any index can be kept on a phone.
func (s *Server) On() bool { return s != nil && len(s.sources) > 0 }
