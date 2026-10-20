package media

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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

	mu    sync.RWMutex
	items map[string]*Item // by sha256

	// Held while a file's upload is started, written to or finished, so the same file arriving from
	// two devices (or a retry racing the first try) is written once.
	uploadLocks keyedMutex

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
	if err := s.scan(); err != nil {
		return nil, fmt.Errorf("index %s: %w", root, err)
	}
	return s, nil
}

func (s *Store) Root() string { return s.root }

func (s *Store) uploadsDir() string { return filepath.Join(s.root, ".cache", "uploads") }

// scan reads every sidecar under media/. A sidecar that can't be read, or whose original is
// missing, is logged and skipped: it is left on disk for the owner to look at.
func (s *Store) scan() error {
	mediaDir := filepath.Join(s.root, "media")
	return filepath.WalkDir(mediaDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var sidecar Sidecar
		if err := json.Unmarshal(data, &sidecar); err != nil || sidecar.Format != SidecarFormat || !IsSha256(sidecar.Sha256) {
			// Also an original that happens to be a .json file; its sidecar is <name>.json.json.
			return nil
		}
		original := strings.TrimSuffix(path, ".json")
		if info, err := os.Stat(original); err != nil || !info.Mode().IsRegular() {
			log.Printf("media: %s has no original next to it; skipped", path)
			return nil
		}
		rel, err := filepath.Rel(s.root, original)
		if err != nil {
			return err
		}
		if existing, ok := s.items[sidecar.Sha256]; ok {
			log.Printf("media: %s is a second copy of %s; skipped", rel, existing.Path)
			return nil
		}
		s.items[sidecar.Sha256] = &Item{Path: filepath.ToSlash(rel), Sidecar: sidecar}
		return nil
	})
}

func (s *Store) Get(sha256 string) (Item, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, ok := s.items[sha256]
	if !ok {
		return Item{}, false
	}
	return *item, true
}

// Missing returns the hashes the store does not have, in the order given, without repeats.
func (s *Store) Missing(hashes []string) []string {
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

// keyedMutex is a mutex per key, dropped again when nobody holds or waits for it.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	sync.Mutex
	refs int
}

func (k *keyedMutex) Lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedLock{}
	}
	lock, ok := k.locks[key]
	if !ok {
		lock = &keyedLock{}
		k.locks[key] = lock
	}
	lock.refs++
	k.mu.Unlock()

	lock.Lock()
	return func() {
		lock.Unlock()
		k.mu.Lock()
		if lock.refs--; lock.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

// Library is the stores of all users that have a data folder.
type Library struct {
	stores map[string]*Store // by username
}

// OpenLibrary opens a store for each username → data folder.
func OpenLibrary(folders map[string]string, now func() time.Time) (*Library, error) {
	library := &Library{stores: map[string]*Store{}}
	for user, folder := range folders {
		store, err := OpenStore(folder, now)
		if err != nil {
			return nil, fmt.Errorf("user %s: %w", user, err)
		}
		library.stores[user] = store
	}
	return library, nil
}

// Store is nil when the user has no data folder configured.
func (l *Library) Store(user string) *Store {
	if l == nil {
		return nil
	}
	return l.stores[user]
}

// PurgeExpiredUploads deletes unfinished uploads that were not continued in time.
func (l *Library) PurgeExpiredUploads() {
	for user, store := range l.stores {
		if err := store.PurgeExpiredUploads(); err != nil {
			log.Printf("media: purging expired uploads of %s: %v", user, err)
		}
	}
}

// Changes returns up to limit originals uploaded after cursor ("" for all), oldest upload first, with the cursor
// to continue from. Originals never change, so their upload order is the order of changes; the cursor is
// opaque to clients, so the feed can later count edits to notes and stories too.
func (s *Store) Changes(cursor string, limit int) (items []Item, next string, hasMore bool, err error) {
	after, err := decodeCursor(cursor)
	if err != nil {
		return nil, "", false, err
	}
	s.mu.RLock()
	all := make([]Item, 0, len(s.items))
	for _, item := range s.items {
		if after.before(*item) {
			all = append(all, *item)
		}
	}
	s.mu.RUnlock()
	sort.Slice(all, func(i, j int) bool { return position(all[i]).less(position(all[j])) })
	if len(all) > limit {
		all, hasMore = all[:limit], true
	}
	next = cursor
	if len(all) > 0 {
		next = position(all[len(all)-1]).encode()
	}
	return all, next, hasMore, nil
}

// Usage is what the originals take up: how many photos and videos, and their bytes. Sidecars, uploads in progress
// and previews are left out; they are small, or rebuilt.
type Usage struct {
	Photos, Videos         int
	PhotoBytes, VideoBytes int64
}

func (s *Store) Usage() Usage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var u Usage
	for _, item := range s.items {
		facts := item.Sidecar.Facts
		if facts.Kind == KindVideo {
			u.Videos++
			u.VideoBytes += facts.Size
		} else {
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

// feedPosition orders the feed: upload time, then sha256 for files uploaded in the same instant.
type feedPosition struct {
	uploadedAt int64
	sha        string
}

func position(item Item) feedPosition {
	return feedPosition{item.Sidecar.Facts.UploadedAt.UnixNano(), item.Sidecar.Sha256}
}

func (p feedPosition) less(o feedPosition) bool {
	return p.uploadedAt < o.uploadedAt || p.uploadedAt == o.uploadedAt && p.sha < o.sha
}

// before reports whether item comes after p; the zero position comes before everything.
func (p *feedPosition) before(item Item) bool { return p == nil || p.less(position(item)) }

func (p feedPosition) encode() string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("m1:%d:%s", p.uploadedAt, p.sha)))
}

// ErrBadCursor is a cursor this server didn't hand out.
var ErrBadCursor = errors.New("unknown cursor; start again without one")

func decodeCursor(cursor string) (*feedPosition, error) {
	if cursor == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, ErrBadCursor
	}
	var p feedPosition
	if _, err := fmt.Sscanf(string(raw), "m1:%d:%s", &p.uploadedAt, &p.sha); err != nil || !IsSha256(p.sha) {
		return nil, ErrBadCursor
	}
	return &p, nil
}
