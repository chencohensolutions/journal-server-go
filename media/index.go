package media

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Reading every sidecar takes minutes on a spinning disk (a seek each), so what scan found is kept in
// .cache/index.json, folder by folder, with each folder's modified time when it was listed. The next scan
// reads a folder's sidecars again only when that time has changed. A folder's time changes whenever a file
// in it is added, removed or renamed, and every write here ends in a rename (writeFileAtomic), so a placed
// file or a rewritten sidecar is always seen. A sidecar edited in place by hand is not: delete the cache to
// read everything again. The cache is only a shortcut; the sidecars stay the truth.

// indexFormat changes when the cache's shape does; a cache of another format is ignored.
const indexFormat = 1

// scanReaders is how many sidecars scan reads at once: even one spinning disk serves a queue of reads in a
// better order than one read at a time.
const scanReaders = 16

type indexCache struct {
	Format  int                     `json:"format"`
	Folders map[string]*indexFolder `json:"folders"` // by path relative to the data folder: "media/2026/2026-10"
}

type indexFolder struct {
	Modified time.Time      `json:"modified"`
	Folders  []string       `json:"folders,omitempty"` // names of the folders inside it
	Sidecars []indexSidecar `json:"sidecars,omitempty"`
}

// indexSidecar is a sidecar as it was on disk. Data is left out of one that isn't indexed (not a sidecar, or
// its original is missing), so it is skipped again without being read.
type indexSidecar struct {
	Name string          `json:"name"`
	Data json.RawMessage `json:"data,omitempty"`

	fresh    bool // read from disk by this scan, not taken from the cache
	original bool // when fresh: its original was next to it when the folder was listed
	orphan   bool // a sidecar whose original is missing
	err      error
}

func (s *Store) indexPath() string { return filepath.Join(s.root, ".cache", "index.json") }

func (s *Store) readIndexCache() map[string]*indexFolder {
	data, err := os.ReadFile(s.indexPath())
	if err != nil {
		return nil
	}
	var cache indexCache
	if err := json.Unmarshal(data, &cache); err != nil || cache.Format != indexFormat {
		log.Printf("media: %s is unreadable or of another format; reading every sidecar", s.indexPath())
		return nil
	}
	return cache.Folders
}

// indexed is one sidecar the walk found, with its original's path relative to the data folder.
type indexed struct {
	path    string
	sidecar Sidecar
}

// walkIndex finds the sidecars under media/, taking unchanged folders from the cache and reading the rest
// from disk, and writes the cache again when anything changed. Sidecars come in walk order (each folder's
// files, then the folders inside it, by name), so which of two copies scan keeps doesn't depend on timing.
func (s *Store) walkIndex() ([]indexed, error) {
	cached := s.readIndexCache()
	folders := map[string]*indexFolder{}
	var order []string
	var unread []*indexSidecar
	var unreadPaths []string
	var visit func(rel string) error
	visit = func(rel string) error {
		dir := filepath.Join(s.root, filepath.FromSlash(rel))
		info, err := os.Stat(dir)
		if err != nil {
			return err
		}
		folder := cached[rel]
		// The time is taken before listing, so a change made while listing shows as a change next time.
		if folder == nil || !folder.Modified.Equal(info.ModTime()) {
			entries, err := os.ReadDir(dir)
			if err != nil {
				return err
			}
			folder = &indexFolder{Modified: info.ModTime()}
			files := map[string]bool{}
			for _, entry := range entries {
				if entry.Type().IsRegular() {
					files[entry.Name()] = true
				}
			}
			for _, entry := range entries {
				name := entry.Name()
				if entry.IsDir() {
					folder.Folders = append(folder.Folders, name)
				} else if strings.HasSuffix(name, ".json") {
					original := files[strings.TrimSuffix(name, ".json")]
					folder.Sidecars = append(folder.Sidecars, indexSidecar{Name: name, fresh: true, original: original})
				}
			}
			for i := range folder.Sidecars {
				unread = append(unread, &folder.Sidecars[i])
				unreadPaths = append(unreadPaths, filepath.Join(dir, folder.Sidecars[i].Name))
			}
		}
		folders[rel] = folder
		order = append(order, rel)
		for _, name := range folder.Folders {
			if err := visit(rel + "/" + name); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit("media"); err != nil {
		return nil, err
	}

	parallel(len(unread), scanReaders, func(i int) {
		unread[i].Data, unread[i].err = os.ReadFile(unreadPaths[i])
	})
	var all []*indexSidecar
	var paths []string
	for _, rel := range order {
		folder := folders[rel]
		for i := range folder.Sidecars {
			all = append(all, &folder.Sidecars[i])
			paths = append(paths, rel+"/"+strings.TrimSuffix(folder.Sidecars[i].Name, ".json"))
		}
	}
	for _, sidecar := range unread {
		if sidecar.err != nil {
			return nil, sidecar.err
		}
	}

	parsed := make([]*Sidecar, len(all))
	parallel(len(all), runtime.NumCPU(), func(i int) {
		entry := all[i]
		if entry.Data == nil {
			return
		}
		var sidecar Sidecar
		// Also an original that happens to be a .json file; its sidecar is <name>.json.json.
		if err := json.Unmarshal(entry.Data, &sidecar); err != nil || sidecar.Format != SidecarFormat || !IsSha256(sidecar.Sha256) {
			entry.Data = nil
			return
		}
		if entry.fresh && !entry.original {
			entry.orphan = true
			entry.Data = nil
			return
		}
		parsed[i] = &sidecar
	})

	var found []indexed
	for i, entry := range all {
		if entry.orphan {
			log.Printf("media: %s has no original next to it; skipped", filepath.Join(s.root, filepath.FromSlash(paths[i]))+".json")
		}
		if parsed[i] != nil {
			found = append(found, indexed{path: paths[i], sidecar: *parsed[i]})
		}
	}

	if (len(unread) > 0 || len(folders) != len(cached)) && !s.readOnly {
		data, err := json.Marshal(indexCache{Format: indexFormat, Folders: folders})
		if err == nil {
			err = writeFileAtomic(s.indexPath(), data)
		}
		if err != nil {
			log.Printf("media: saving %s: %v", s.indexPath(), err)
		}
	}
	return found, nil
}

// scan indexes every sidecar under media/. A sidecar that can't be read, or whose original is missing, is
// skipped (and logged when first found): it is left on disk for the owner to look at.
func (s *Store) scan() error {
	found, err := s.walkIndex()
	if err != nil {
		return err
	}
	for _, f := range found {
		if existing, ok := s.items[f.sidecar.Sha256]; ok {
			log.Printf("media: %s is a second copy of %s; skipped", f.path, existing.Path)
			continue
		}
		s.indexItem(&Item{Path: f.path, Sidecar: f.sidecar})
	}
	return nil
}

// RefreshIndexCache brings .cache/index.json up to date with the data folder, reading only the folders
// that changed, so the next start (of the server or a script) is quick. The index in memory is not touched.
func (s *Store) RefreshIndexCache() error {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	_, err := s.walkIndex()
	return err
}

// RefreshIndexCacheIfChanged refreshes .cache/index.json when this process has placed a file or rewritten a
// sidecar since it was last saved: the nudity scan rewrites a sidecar for every file it reads, and each one makes
// its folder read again at the next start (minutes for a library on a spinning disk) until the cache is saved.
func (s *Store) RefreshIndexCacheIfChanged() error {
	if !s.indexStale.Swap(false) {
		return nil
	}
	if err := s.RefreshIndexCache(); err != nil {
		s.indexStale.Store(true)
		return err
	}
	return nil
}

// parallel calls fn(0) … fn(n-1) on up to workers goroutines at once.
func parallel(n, workers int, fn func(i int)) {
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(workers, n) {
		wg.Go(func() {
			for i := range next {
				fn(i)
			}
		})
	}
	for i := range n {
		next <- i
	}
	close(next)
	wg.Wait()
}
