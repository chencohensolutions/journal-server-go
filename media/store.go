package media

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Store is one user's data folder:
//
//	media/2026/2026-10/IMG_1234.HEIC        the original, never modified
//	media/2026/2026-10/IMG_1234.HEIC.json   its sidecar
//	.cache/uploads/<sha256>.partial         an upload in progress, with <sha256>.json beside it
type Store struct {
	root string
	now  func() time.Time

	mu     sync.RWMutex
	items  map[string]*Item // by sha256
	hidden Hidden           // what the user hides on every device (hidden.go)

	// Held while a file's upload is started, written to or finished, so the same file arriving from
	// two devices (or a retry racing the first try) is written once.
	uploadLocks keyedMutex

	// Taking turns with other processes that have this data folder open (turns.go).
	turns turns

	// This process has placed a file or rewritten a sidecar since .cache/index.json was last saved, so the next
	// start would read those folders again (RefreshIndexCacheIfChanged).
	indexStale atomic.Bool
	// One refresh of .cache/index.json at a time.
	indexMu sync.Mutex

	// Keep previews (and developed DNGs) in .cache/previews; else each is made, sent and deleted.
	// Set once, before the store serves requests (config.json "preview_cache").
	CachePreviews bool
}

// OpenStore creates the folders it needs and indexes the sidecars already in root.
func OpenStore(root string, now func() time.Time) (*Store, error) {
	s := &Store{root: root, now: now, items: map[string]*Item{}}
	for _, dir := range []string{filepath.Join(root, "media"), s.uploadsDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	hidden, err := s.readHidden()
	if err != nil {
		return nil, err
	}
	s.hidden = hidden
	s.startJournal()
	log.Printf("media: indexing %s", root)
	started := time.Now()
	if err := s.scan(); err != nil {
		return nil, fmt.Errorf("index %s: %w", root, err)
	}
	log.Printf("media: indexed %d files in %s after %s", len(s.items), root, time.Since(started).Round(time.Millisecond))
	return s, nil
}

func (s *Store) Root() string { return s.root }

func (s *Store) uploadsDir() string { return filepath.Join(s.root, ".cache", "uploads") }

func (s *Store) Get(sha256 string) (Item, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, ok := s.items[sha256]
	if !ok {
		return Item{}, false
	}
	return *item, true
}

// Range calls fn with each original the store knows, in no order, after catching up with the other processes.
// fn runs while the index is locked for reading, so it must not call the store.
func (s *Store) Range(fn func(Item)) {
	s.CatchUp()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range s.items {
		fn(*item)
	}
}

// Missing returns the hashes the store does not have, in the order given, without repeats.
func (s *Store) Missing(hashes []string) []string {
	s.CatchUp()
	s.mu.RLock()
	defer s.mu.RUnlock()
	missing := []string{}
	seen := map[string]bool{}
	for _, h := range hashes {
		if _, ok := s.items[h]; !ok && !seen[h] {
			missing = append(missing, h)
		}
		seen[h] = true
	}
	return missing
}

// Related lists the edits of sha256 and the files paired with it, oldest upload first.
func (s *Store) Related(sha256 string) (versions, paired []string) {
	s.mu.RLock()
	var versionItems, pairedItems []*Item
	for _, item := range s.items {
		facts := item.Sidecar.Facts
		if facts.VersionOf != nil && *facts.VersionOf == sha256 {
			versionItems = append(versionItems, item)
		}
		if facts.PairedWith != nil && *facts.PairedWith == sha256 {
			pairedItems = append(pairedItems, item)
		}
	}
	s.mu.RUnlock()
	return byUploadTime(versionItems), byUploadTime(pairedItems)
}

func byUploadTime(items []*Item) []string {
	sort.Slice(items, func(i, j int) bool {
		return items[i].Sidecar.Facts.UploadedAt.Before(items[j].Sidecar.Facts.UploadedAt)
	})
	hashes := make([]string, len(items))
	for i, item := range items {
		hashes[i] = item.Sidecar.Sha256
	}
	return hashes
}

// Open opens an original for reading.
func (s *Store) Open(item Item) (*os.File, error) {
	return os.Open(filepath.Join(s.root, filepath.FromSlash(item.Path)))
}

// Usage is what the originals take up: how many photos and videos, and their bytes. Sidecars, uploads in progress
// and previews are left out; they are small, or rebuilt.
type Usage struct {
	Photos, Videos, Audio              int
	PhotoBytes, VideoBytes, AudioBytes int64
}

// Usage counts the originals the view shows (View{} for all of them).
func (s *Store) Usage(v View) Usage {
	s.CatchUp()
	s.mu.RLock()
	defer s.mu.RUnlock()
	var u Usage
	for _, item := range s.items {
		if s.hides(v, *item) {
			continue
		}
		facts := item.Sidecar.Facts
		switch facts.Kind {
		case KindVideo:
			u.Videos++
			u.VideoBytes += facts.Size
		case KindAudio:
			u.Audio++
			u.AudioBytes += facts.Size
		default:
			u.Photos++
			u.PhotoBytes += facts.Size
		}
	}
	return u
}

// RelatedIndex is Related for every original at once: edits and paired files by the sha256 they point at.
func (s *Store) RelatedIndex() (versions, paired map[string][]string) {
	s.mu.RLock()
	var versionItems, pairedItems = map[string][]*Item{}, map[string][]*Item{}
	for _, item := range s.items {
		if v := item.Sidecar.Facts.VersionOf; v != nil {
			versionItems[*v] = append(versionItems[*v], item)
		}
		if p := item.Sidecar.Facts.PairedWith; p != nil {
			pairedItems[*p] = append(pairedItems[*p], item)
		}
	}
	s.mu.RUnlock()
	versions, paired = map[string][]string{}, map[string][]string{}
	for sha, items := range versionItems {
		versions[sha] = byUploadTime(items)
	}
	for sha, items := range pairedItems {
		paired[sha] = byUploadTime(items)
	}
	return versions, paired
}
