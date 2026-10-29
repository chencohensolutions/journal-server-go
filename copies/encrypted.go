package copies

import (
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
	"github.com/journal/server-go/media"
	"golang.org/x/crypto/chacha20poly1305"
)

// An encrypted copy (docs/peers.md, "Encrypted copies") keeps each file at the peer encrypted with age to the user's
// copy key, under a name the peer can't read: blobs/<2 hex>/<HMAC-SHA256(naming key, lower-case path) in hex>. The
// private half of the key is in a key file the owner keeps (copy-key new); this server keeps only the public half
// and the naming key (CopyKey), so it can write the copy but never read it back. What the copy holds is in its
// manifest, kept here and, sealed with a key from the naming key, at the peer.

// CopyKey is what this server keeps of a user's copy key, in copy_keys_dir/<user>.json.
type CopyKey struct {
	V    int    `json:"v"`
	User string `json:"user"`
	// The age public key each file is encrypted to.
	Recipient string `json:"recipient"`
	// Names the files at the peer, and seals the manifest.
	NamingKey []byte    `json:"naming_key"`
	CreatedAt time.Time `json:"created_at"`
	// Where copy-key new wrote the key file, to remind the owner; "" for a key from a passphrase.
	KeyFile string `json:"key_file,omitempty"`
	// How the key comes from its passphrase (copy-key new --from-passphrase); nil for a key file's.
	Derived *Derivation `json:"derived,omitempty"`
}

// NewCopyKey makes a user's copy key: what this server keeps, and the identity only the key file holds.
func NewCopyKey(user string, now time.Time) (CopyKey, *age.X25519Identity, error) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return CopyKey{}, nil, err
	}
	naming := make([]byte, 32)
	if _, err := rand.Read(naming); err != nil {
		return CopyKey{}, nil, err
	}
	return CopyKey{V: 1, User: user, Recipient: identity.Recipient().String(), NamingKey: naming, CreatedAt: now.UTC()}, identity, nil
}

// BlobName is where the file at rel (a data folder path) is kept in an encrypted copy.
func (k CopyKey) BlobName(rel string) string {
	mac := hmac.New(sha256.New, k.NamingKey)
	mac.Write([]byte(strings.ToLower(rel)))
	name := hex.EncodeToString(mac.Sum(nil))
	return "blobs/" + name[:2] + "/" + name
}

// manifestKey seals the manifest at the peer: this server and the key file can open it, the peer can't.
func (k CopyKey) manifestKey() []byte {
	mac := hmac.New(sha256.New, k.NamingKey)
	mac.Write([]byte("journal copy manifest v1"))
	return mac.Sum(nil)
}

// errNoKey is a read of an encrypted copy's file: only the key file can.
var errNoKey = errors.New("an encrypted copy can't be read on this server: it needs the key file")

// manifestNames are the manifest's two places at the peer, written in turn, so one stays whole while the other is
// written; the newer is read.
var manifestNames = [2]string{"manifest-a.sealed", "manifest-b.sealed"}

// pushEvery is how often a realtime batch also sends the manifest to the peer; a full pass always does.
var pushEvery = time.Hour

// Entry is one file of an encrypted copy, as its manifest records it.
type Entry struct {
	Path    string    `json:"path"` // in the data folder
	Name    string    `json:"name"` // at the peer
	Size    int64     `json:"size"`
	Sha256  string    `json:"sha256"`
	ModTime time.Time `json:"modTime"`
	Stored  int64     `json:"stored"` // its encrypted size at the peer
}

type manifest struct {
	V     int       `json:"v"`
	User  string    `json:"user"`
	Seq   int64     `json:"seq"`
	Files []Entry   `json:"files"`
	At    time.Time `json:"at"`
}

// Encrypted is an encrypted copy on a peer (Inner): the Target the copier writes to.
type Encrypted struct {
	Inner *Peer
	key   CopyKey
	to    age.Recipient
	dir   string // this server's own folder for the copy: the manifest, .journal-copy.json, files being sent

	mu       sync.Mutex
	files    map[string]Entry // by lower-case path
	seq      int64
	dirty    bool // the manifest changed since it was saved here
	unpushed bool // since it was sent to the peer
	pushedAt time.Time
	salted   bool // copy-key.json is at the peer (a key from a passphrase)
}

// OpenEncrypted is an encrypted copy on inner, keeping its own state in dir. Its manifest is read from dir, or from
// the peer when dir has none (this server set up again).
func OpenEncrypted(inner *Peer, key CopyKey, dir string) (*Encrypted, error) {
	to, err := age.ParseX25519Recipient(key.Recipient)
	if err != nil {
		return nil, fmt.Errorf("copy key of %s: %w", key.User, err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sending"), 0o700); err != nil {
		return nil, err
	}
	e := &Encrypted{Inner: inner, key: key, to: to, dir: dir, files: map[string]Entry{}}
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	switch {
	case err == nil:
		var m manifest
		if err := json.Unmarshal(data, &m); err != nil || m.V != 1 {
			return nil, fmt.Errorf("%s isn't readable: %v", filepath.Join(dir, "manifest.json"), err)
		}
		e.use(m)
	case errors.Is(err, fs.ErrNotExist):
		// Not here: the peer's, if it can be reached now; otherwise the first pass finds the copy empty and sends
		// everything again under the same names.
		if m, err := e.fetchManifest(); err == nil {
			e.use(m)
			e.dirty = true
		}
	default:
		return nil, err
	}
	e.cleanSending()
	return e, nil
}

func (e *Encrypted) use(m manifest) {
	e.seq = m.Seq
	for _, f := range m.Files {
		e.files[strings.ToLower(f.Path)] = f
	}
}

// cleanSending removes files left being sent more than a day ago.
func (e *Encrypted) cleanSending() {
	entries, _ := os.ReadDir(filepath.Join(e.dir, "sending"))
	for _, d := range entries {
		if info, err := d.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
			os.Remove(filepath.Join(e.dir, "sending", d.Name()))
		}
	}
}

func (e *Encrypted) Ready() error                  { return e.Inner.Ready() }
func (e *Encrypted) Free(p string) (uint64, error) { return e.Inner.Free(p) }
func (e *Encrypted) local(name string) string      { return filepath.Join(e.dir, filepath.FromSlash(name)) }
func isLocal(rel string) bool                      { return rel == savedName }

func (e *Encrypted) Stat(rel string) (File, error) {
	if isLocal(rel) {
		return Folder(e.dir).Stat(rel)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	f, ok := e.files[strings.ToLower(rel)]
	if !ok {
		return File{}, fmt.Errorf("%s: %w", rel, fs.ErrNotExist)
	}
	return File{Path: f.Path, Size: f.Size, ModTime: f.ModTime}, nil
}

func (e *Encrypted) List(dir string) ([]File, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var files []File
	for _, f := range e.files {
		if path.Dir(f.Path) == dir {
			files = append(files, File{Path: f.Path, Size: f.Size, ModTime: f.ModTime})
		}
	}
	return files, nil
}

// Verify checks the manifest against what the peer holds (a compare and a full pass start with it): a file whose
// blob is gone, or isn't the size sent, is dropped from it, so it is found missing and sent again.
func (e *Encrypted) Verify() error {
	stored := map[string]int64{}
	if err := e.Inner.Walk("blobs", func(f File) error { stored[f.Path] = f.Size; return nil }); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for key, f := range e.files {
		if size, ok := stored[f.Name]; !ok || size != f.Stored {
			delete(e.files, key)
			e.dirty, e.unpushed = true, true
		}
	}
	return nil
}

func (e *Encrypted) Walk(dir string, fn func(File) error) error {
	e.mu.Lock()
	var files []File
	for _, f := range e.files {
		if strings.HasPrefix(f.Path, dir+"/") {
			files = append(files, File{Path: f.Path, Size: f.Size, ModTime: f.ModTime})
		}
	}
	e.mu.Unlock()
	for _, f := range files {
		if err := fn(f); err != nil {
			return err
		}
	}
	return nil
}

func (e *Encrypted) Open(rel string) (io.ReadCloser, error) {
	if isLocal(rel) {
		return Folder(e.dir).Open(rel)
	}
	return nil, errNoKey
}

// Sha256 is a file's sha256 and size as the manifest records them (remoteHasher).
func (e *Encrypted) Sha256(rel string) (string, int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f, ok := e.files[strings.ToLower(rel)]
	if !ok {
		return "", 0, fmt.Errorf("%s: %w", rel, fs.ErrNotExist)
	}
	return f.Sha256, f.Size, nil
}

func (e *Encrypted) Put(rel string, r io.Reader, size int64, sum string, modTime time.Time) error {
	return e.PutProgress(rel, r, size, sum, modTime, nil)
}

// PutProgress encrypts the file into this server's folder first (kept by its sha256 until it is sent, so a send
// cut off resumes with the same bytes), then sends it. progress gets the bytes as they are sent, counted in the
// file's own size; a write to it that fails (the pass stopped) ends the put, in encrypting too.
func (e *Encrypted) PutProgress(rel string, r io.Reader, size int64, sum string, modTime time.Time, progress io.Writer) error {
	if isLocal(rel) {
		return Folder(e.dir).Put(rel, r, size, sum, modTime)
	}
	sealed := filepath.Join(e.dir, "sending", sum+".age")
	if _, err := os.Stat(sealed); err != nil {
		if err := e.seal(r, size, sum, sealed, progress); err != nil {
			return err
		}
	}
	f, err := os.Open(sealed)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	stored, err := io.Copy(hash, f)
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var body io.Reader = f
	if progress != nil {
		body = io.TeeReader(f, &scaled{w: progress, of: size, from: stored})
	}
	name := e.key.BlobName(rel)
	// The blob's own time is when it was sent, not the file's: the peer learns nothing from it.
	if err := e.Inner.Put(name, body, stored, hex.EncodeToString(hash.Sum(nil)), time.Now()); err != nil {
		return err
	}
	f.Close()
	os.Remove(sealed)
	e.mu.Lock()
	e.files[strings.ToLower(rel)] = Entry{Path: rel, Name: name, Size: size, Sha256: sum, ModTime: modTime, Stored: stored}
	e.dirty, e.unpushed = true, true
	e.mu.Unlock()
	return nil
}

// seal encrypts r to the copy key into file, checking r's bytes against sum on the way.
func (e *Encrypted) seal(r io.Reader, size int64, sum, file string, progress io.Writer) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), ".sealing-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	w, err := age.Encrypt(tmp, e.to)
	if err != nil {
		tmp.Close()
		return err
	}
	if progress != nil {
		r = io.TeeReader(r, checkOnly{progress})
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, hash), r)
	if err == nil {
		err = w.Close()
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != sum || n != size {
		return fmt.Errorf("%w: %d bytes with sha256 %s, not %d with %s", errDamaged, n, got, size, sum)
	}
	return media.Rename(tmp.Name(), file)
}

// checkOnly lets a progress writer stop the encrypting (its error) without counting it: sending is what counts.
type checkOnly struct{ w io.Writer }

func (c checkOnly) Write(b []byte) (int, error) {
	if _, err := c.w.Write(b[:0]); err != nil {
		return 0, err
	}
	return len(b), nil
}

// scaled counts encrypted bytes sent as bytes of the file: what the pass's progress is in.
type scaled struct {
	w        io.Writer
	of, from int64
	sent     int64 // encrypted
	counted  int64 // of the file
}

func (s *scaled) Write(b []byte) (int, error) {
	s.sent += int64(len(b))
	want := s.of
	if s.from > 0 {
		want = min(s.of, s.sent*s.of/s.from)
	}
	if n := want - s.counted; n > 0 {
		if _, err := s.w.Write(make([]byte, n)); err != nil {
			return 0, err
		}
		s.counted = want
	} else if _, err := s.w.Write(b[:0]); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Touch changes a file's time in the manifest only: the peer's blob doesn't carry it.
func (e *Encrypted) Touch(rel string, modTime time.Time) error {
	if isLocal(rel) {
		return Folder(e.dir).Touch(rel, modTime)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	f, ok := e.files[strings.ToLower(rel)]
	if !ok {
		return fmt.Errorf("%s: %w", rel, fs.ErrNotExist)
	}
	f.ModTime = modTime
	e.files[strings.ToLower(rel)] = f
	e.dirty, e.unpushed = true, true
	return nil
}

func (e *Encrypted) Remove(rel string) error {
	if isLocal(rel) {
		return Folder(e.dir).Remove(rel)
	}
	e.mu.Lock()
	f, ok := e.files[strings.ToLower(rel)]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("%s: %w", rel, fs.ErrNotExist)
	}
	if err := e.Inner.Remove(f.Name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	e.mu.Lock()
	delete(e.files, strings.ToLower(rel))
	e.dirty, e.unpushed = true, true
	e.mu.Unlock()
	return nil
}

// Move moves the file's blob to the name of to, and its entry with it.
func (e *Encrypted) Move(from, to string) error {
	if isLocal(from) || isLocal(to) {
		return Folder(e.dir).Move(from, to)
	}
	e.mu.Lock()
	f, ok := e.files[strings.ToLower(from)]
	_, taken := e.files[strings.ToLower(to)]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("%s: %w", from, fs.ErrNotExist)
	}
	if taken {
		return fmt.Errorf("%s is there already", to)
	}
	name := e.key.BlobName(to)
	if err := e.Inner.Move(f.Name, name); err != nil {
		return err
	}
	e.mu.Lock()
	delete(e.files, strings.ToLower(from))
	f.Path, f.Name = to, name
	e.files[strings.ToLower(to)] = f
	e.dirty, e.unpushed = true, true
	e.mu.Unlock()
	return nil
}

// Files is the manifest's count of files.
func (e *Encrypted) Files() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.files)
}

// Flush saves the manifest here, and sends it to the peer after a full pass, or when it was last sent pushEvery ago.
func (e *Encrypted) Flush(full bool) error {
	e.mu.Lock()
	m := manifest{V: 1, User: e.key.User, Seq: e.seq, At: time.Now().UTC()}
	for _, f := range e.files {
		m.Files = append(m.Files, f)
	}
	dirty, push := e.dirty, e.unpushed && (full || time.Since(e.pushedAt) > pushEvery)
	e.dirty = false
	e.mu.Unlock()
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	if dirty {
		data, err := json.Marshal(m)
		if err != nil {
			return err
		}
		if err := writeAtomic(filepath.Join(e.dir, "manifest.json"), data); err != nil {
			e.mu.Lock()
			e.dirty = true
			e.mu.Unlock()
			return err
		}
	}
	if e.key.Derived != nil && !e.salted && full {
		// The salt beside the copy, so the passphrase alone brings it back.
		data, _ := json.Marshal(map[string]any{"v": 1, "user": e.key.User, "derivation": e.key.Derived})
		sum := sha256.Sum256(data)
		if err := e.Inner.Put(derivationName, bytes.NewReader(data), int64(len(data)), hex.EncodeToString(sum[:]), time.Now()); err != nil {
			return err
		}
		e.salted = true
	}
	if !push {
		return nil
	}
	m.Seq++
	sealed, err := e.sealManifest(m)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(sealed)
	if err := e.Inner.Put(manifestNames[m.Seq%2], bytes.NewReader(sealed), int64(len(sealed)), hex.EncodeToString(sum[:]), time.Now()); err != nil {
		return err
	}
	e.mu.Lock()
	e.seq, e.unpushed, e.pushedAt = m.Seq, false, time.Now()
	e.mu.Unlock()
	return nil
}

// sealManifest is the manifest gzipped and sealed (XChaCha20-Poly1305) with the manifest key: a random nonce, then
// the sealed bytes.
func (e *Encrypted) sealManifest(m manifest) ([]byte, error) {
	var plain bytes.Buffer
	zw := gzip.NewWriter(&plain)
	if err := json.NewEncoder(zw).Encode(m); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(e.key.manifestKey())
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	rand.Read(nonce)
	return aead.Seal(nonce, nonce, plain.Bytes(), []byte(e.key.User)), nil
}

// OpenManifest opens a sealed manifest with the naming key (this server's, or the key file's).
func OpenManifest(key CopyKey, sealed []byte) ([]Entry, int64, error) {
	m, err := openManifest(key, sealed)
	return m.Files, m.Seq, err
}

func openManifest(key CopyKey, sealed []byte) (manifest, error) {
	aead, err := chacha20poly1305.NewX(key.manifestKey())
	if err != nil {
		return manifest{}, err
	}
	if len(sealed) < aead.NonceSize() {
		return manifest{}, errors.New("the manifest is cut short")
	}
	plain, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], []byte(key.User))
	if err != nil {
		return manifest{}, fmt.Errorf("the manifest can't be opened with this key: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		return manifest{}, err
	}
	var m manifest
	if err := json.NewDecoder(zr).Decode(&m); err != nil || m.V != 1 {
		return manifest{}, fmt.Errorf("the manifest isn't readable: %v", err)
	}
	return m, nil
}

// fetchManifest is the newer of the peer's two manifests.
func (e *Encrypted) fetchManifest() (manifest, error) {
	var newest manifest
	found := false
	var last error
	for _, name := range manifestNames {
		sealed, err := readAll(e.Inner, name)
		if err != nil {
			last = err
			continue
		}
		m, err := openManifest(e.key, sealed)
		if err != nil {
			last = err
			continue
		}
		if !found || m.Seq > newest.Seq {
			newest, found = m, true
		}
	}
	if !found {
		return manifest{}, last
	}
	return newest, nil
}

func writeAtomic(file string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), "."+filepath.Base(file)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return media.Rename(tmp.Name(), file)
}
