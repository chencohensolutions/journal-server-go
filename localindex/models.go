package localindex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// hashesFile, in a model folder, keeps the sha256 of each file phones download, by its size and modified time, so
// a restart doesn't read gigabytes again.
const hashesFile = ".hashes.json"

type hashed struct {
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
	Sha256  string    `json:"sha256"`
}

// ModelFile is a model file a phone may download, once its sha256 is known.
type ModelFile struct {
	Index     string
	Name      string
	Precision string
	Bytes     int64
	Sha256    string
	// URL is its path on this server: /models/{index}/{name}.
	URL string
	// Source is where the phone may download the same file first (Hugging Face, Google), as the folder's
	// sourcesFile names it with this sha256; "" to download it only from here.
	Source string
}

// sourcesFile, in a model folder, is where each file there was downloaded from (bundle.SourcesFile, written by
// bundle-onnx): {"<name>": {"url", "sha256"}}.
const sourcesFile = ".sources.json"

// modelFiles is the model files of every index, and their hashes as they become known.
type modelFiles struct {
	mu      sync.Mutex
	files   []*modelFile
	hashing bool
}

type modelFile struct {
	ModelFile
	path    string
	modTime time.Time // the file's when it was hashed
}

func newModelFiles() *modelFiles { return &modelFiles{} }

func (m *modelFiles) add(index, folder string, spec ModelFileSpec) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files = append(m.files, &modelFile{
		ModelFile: ModelFile{Index: index, Name: spec.Name, Precision: spec.Precision, URL: "/models/" + index + "/" + spec.Name},
		path:      filepath.Join(folder, spec.Name),
	})
}

// hashAll finds each file's sha256: from its folder's hashesFile when the file is as it was, or by reading it
// (several seconds a gigabyte), then saved there. A file that isn't there is left out.
func (m *modelFiles) hashAll() {
	m.mu.Lock()
	if m.hashing {
		m.mu.Unlock()
		return
	}
	m.hashing = true
	files := append([]*modelFile(nil), m.files...)
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.hashing = false
		m.mu.Unlock()
	}()
	saved := map[string]map[string]hashed{}                         // folder → name → hash
	sources := map[string]map[string]struct{ URL, Sha256 string }{} // folder → name → where it came from
	for _, f := range files {
		info, err := os.Stat(f.path)
		if err != nil {
			continue
		}
		folder := filepath.Dir(f.path)
		if saved[folder] == nil {
			hashes := map[string]hashed{}
			if data, err := os.ReadFile(filepath.Join(folder, hashesFile)); err == nil {
				json.Unmarshal(data, &hashes)
			}
			saved[folder] = hashes
		}
		h, ok := saved[folder][f.Name]
		if !ok || h.Size != info.Size() || !h.ModTime.Equal(info.ModTime()) {
			started := time.Now()
			sum, err := fileSha256(f.path)
			if err != nil {
				log.Printf("localindex: %s: %v", f.path, err)
				continue
			}
			h = hashed{Size: info.Size(), ModTime: info.ModTime(), Sha256: sum}
			saved[folder][f.Name] = h
			if data, err := json.MarshalIndent(saved[folder], "", "  "); err == nil {
				if err := os.WriteFile(filepath.Join(folder, hashesFile), data, 0o644); err != nil {
					log.Printf("localindex: %v", err)
				}
			}
			log.Printf("Phones may download %s (%d MB, hashed in %s)", f.path, info.Size()>>20, time.Since(started).Round(time.Millisecond))
		}
		if sources[folder] == nil {
			named := map[string]struct{ URL, Sha256 string }{}
			if data, err := os.ReadFile(filepath.Join(folder, sourcesFile)); err == nil {
				json.Unmarshal(data, &named)
			}
			sources[folder] = named
		}
		source := ""
		if s, ok := sources[folder][f.Name]; ok && s.Sha256 == h.Sha256 && strings.HasPrefix(s.URL, "https://") {
			source = s.URL
		}
		m.mu.Lock()
		f.Bytes, f.Sha256, f.modTime, f.Source = h.Size, h.Sha256, h.ModTime, source
		m.mu.Unlock()
	}
}

// of is index's model files whose hash is known.
func (m *modelFiles) of(index string) []ModelFile {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ModelFile
	for _, f := range m.files {
		if f.Index == index && f.Sha256 != "" {
			out = append(out, f.ModelFile)
		}
	}
	return out
}

func (m *modelFiles) find(index, name string) (modelFile, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.files {
		if f.Index == index && f.Name == name && f.Sha256 != "" {
			return *f, true
		}
	}
	return modelFile{}, false
}

// ServeModel answers GET /models/{index}/{name}: a model file the manifest lists, with Range (a download of a
// gigabyte resumes where it broke) and its sha256 as the ETag. Only the files named by the indexes are served.
func (s *Server) ServeModel(w http.ResponseWriter, r *http.Request) {
	f, ok := s.models.find(r.PathValue("index"), r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	file, err := os.Open(f.path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() != f.Bytes || !info.ModTime().Equal(f.modTime) {
		// Replaced since it was hashed (bundle-onnx ran again): hash it again before sending it.
		go s.models.hashAll()
		http.Error(w, "the model file is being checked", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("ETag", `"`+f.Sha256+`"`)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", info.ModTime(), file)
}

func fileSha256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
