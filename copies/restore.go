package copies

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"
	"github.com/journal/server-go/media"
)

// Restore brings a copy back into an empty folder (docs/peers.md, "Restore"): from a peer, or from a folder that
// holds a copy (a friend's peers_folder handed over on a disk, or a copy on another disk). A plain copy comes back
// file by file; an encrypted one is read through its manifest with the key file. Each original is checked against
// its sha256, and a restore stopped part way goes on where it stopped when run again.

// KeyFile is what copy-key new wrote: the identity that decrypts a user's encrypted copies, and the naming key.
type KeyFile struct {
	Key      CopyKey
	Identity age.Identity
}

// ReadKeyFile reads a key file; passphrase is asked for when the file is sealed with one.
func ReadKeyFile(data []byte, passphrase func() (string, error)) (KeyFile, error) {
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte(armor.Header)) {
		secret, err := passphrase()
		if err != nil {
			return KeyFile{}, err
		}
		identity, err := age.NewScryptIdentity(secret)
		if err != nil {
			return KeyFile{}, err
		}
		r, err := age.Decrypt(armor.NewReader(bytes.NewReader(data)), identity)
		if err != nil {
			return KeyFile{}, fmt.Errorf("the key file doesn't open with that passphrase: %w", err)
		}
		if data, err = io.ReadAll(r); err != nil {
			return KeyFile{}, err
		}
	}
	var kf KeyFile
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if v, ok := strings.CutPrefix(line, "# journal-user: "); ok {
			kf.Key.User = v
		}
		if v, ok := strings.CutPrefix(line, "# journal-naming-key: "); ok {
			naming, err := base64.StdEncoding.DecodeString(v)
			if err != nil || len(naming) != 32 {
				return KeyFile{}, errors.New("the key file's naming key isn't readable")
			}
			kf.Key.NamingKey = naming
		}
	}
	identities, err := age.ParseIdentities(bytes.NewReader(data))
	if err != nil || len(identities) != 1 {
		return KeyFile{}, fmt.Errorf("the key file holds no key: %v", err)
	}
	if kf.Key.User == "" || kf.Key.NamingKey == nil {
		return KeyFile{}, errors.New("this isn't a journal copy key file (no journal-user or journal-naming-key)")
	}
	kf.Key.V, kf.Identity = 1, identities[0]
	return kf, nil
}

// restoreMark is in the folder while a restore fills it, so running it again goes on there.
const restoreMark = ".journal-restore.json"

// RestoreWorkers is how many files are brought back at once.
var RestoreWorkers = 4

// RestoreTries is how many times a file is tried before it is listed as failed.
var RestoreTries = 3

// RestoreProgress is how a restore is going, for the command's output.
type RestoreProgress struct {
	Files, Done, Skipped, Failed int
	Bytes                        int64
}

// RestoreResult is how a restore ended.
type RestoreResult struct {
	RestoreProgress
	Encrypted bool
	// The files that couldn't be brought back after every try, with why.
	FailedFiles map[string]string
}

// IsEncrypted reports whether the copy in src is encrypted (it has a sealed manifest).
func IsEncrypted(src Target) bool {
	for _, name := range manifestNames {
		if _, err := src.Stat(name); err == nil {
			return true
		}
		if r, err := src.Open(name); err == nil {
			r.Close()
			return true
		}
	}
	return false
}

// Restore brings the copy in src back into folder, which must be empty (or hold a restore stopped part way). key is
// needed for an encrypted copy. report is called as files come back.
func Restore(ctx context.Context, src Target, key *KeyFile, folder string, report func(RestoreProgress)) (RestoreResult, error) {
	return RestoreAsOf(ctx, src, key, folder, time.Time{}, report)
}

// RestoreAsOf is Restore with each file as it was at asOf: a sidecar or root file the copy replaced since comes back
// in the version it kept (history/); the zero time is now. Files added since come back as they are.
func RestoreAsOf(ctx context.Context, src Target, key *KeyFile, folder string, asOf time.Time, report func(RestoreProgress)) (RestoreResult, error) {
	if err := prepareFolder(folder); err != nil {
		return RestoreResult{}, err
	}
	var jobs, versions []restoreJob
	encrypted := IsEncrypted(src)
	if encrypted {
		if key == nil {
			return RestoreResult{}, errors.New("this copy is encrypted: give the key file with --key")
		}
		var err error
		if jobs, versions, err = encryptedJobs(src, key); err != nil {
			return RestoreResult{}, err
		}
	} else {
		var err error
		if jobs, versions, err = plainJobs(src); err != nil {
			return RestoreResult{}, err
		}
	}
	if !asOf.IsZero() {
		jobs = asOfJobs(jobs, versions, asOf)
	}
	result := RestoreResult{Encrypted: encrypted, FailedFiles: map[string]string{}}
	result.Files = len(jobs)
	var mu sync.Mutex
	work := make(chan restoreJob)
	var wg sync.WaitGroup
	for range max(1, RestoreWorkers) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range work {
				skipped, n, err := job.run(ctx, src, key, folder)
				mu.Lock()
				switch {
				case err != nil:
					result.Failed++
					result.FailedFiles[job.path] = err.Error()
				case skipped:
					result.Skipped++
				default:
					result.Done++
					result.Bytes += n
				}
				if report != nil {
					report(result.RestoreProgress) // one at a time
				}
				mu.Unlock()
			}
		}()
	}
	for _, job := range jobs {
		select {
		case work <- job:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(work)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if result.Failed == 0 {
		os.Remove(filepath.Join(folder, restoreMark))
	}
	return result, nil
}

// prepareFolder makes folder, or accepts it empty or holding a restore stopped part way, and marks it.
func prepareFolder(folder string) error {
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(folder)
	if err != nil {
		return err
	}
	marked := false
	for _, e := range entries {
		if e.Name() == restoreMark {
			marked = true
		}
	}
	if len(entries) > 0 && !marked {
		return fmt.Errorf("%s isn't empty: a restore fills an empty folder only (or goes on with one it started)", folder)
	}
	return os.WriteFile(filepath.Join(folder, restoreMark), []byte(`{"v":1}`+"\n"), 0o644)
}

// restoreJob is one file to bring back: from src's path (a plain copy), or from a blob (an encrypted one).
type restoreJob struct {
	path    string    // in the data folder
	from    string    // in a plain copy, when not path (a version in history/)
	stamp   time.Time // when the copy replaced this version; zero for the current one
	blob    string    // in an encrypted copy, or ""
	size    int64     // -1 when not known before
	sha256  string    // "" when not known before (a plain copy's sidecar names it after)
	modTime time.Time
}

// rootRestored are the root files a plain copy brings back beside media/ and relabels/.
var rootRestored = []string{"people.json", "settings.json", "cameras.json", "sound-folders.json"}

func plainJobs(src Target) ([]restoreJob, []restoreJob, error) {
	var jobs, versions []restoreJob
	add := func(f File) error {
		if !strings.HasPrefix(path.Base(f.Path), ".") {
			jobs = append(jobs, restoreJob{path: f.Path, size: f.Size, modTime: f.ModTime})
		}
		return nil
	}
	for _, dir := range []string{"media", "relabels"} {
		if err := src.Walk(dir, add); err != nil {
			return nil, nil, err
		}
	}
	for _, name := range rootRestored {
		if f, err := src.Stat(name); err == nil {
			add(f)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, nil, err
		}
	}
	if len(jobs) == 0 {
		return nil, nil, errors.New("the copy holds no files (is it the copy's folder, with media/ in it?)")
	}
	err := src.Walk(historyDir, func(f File) error {
		if stamp, name, ok := ParseHistoryPath(f.Path); ok {
			versions = append(versions, restoreJob{path: name, from: f.Path, stamp: stamp, size: f.Size, modTime: f.ModTime})
		}
		return nil
	})
	return jobs, versions, err
}

// asOfJobs replaces each job by the version it had at asOf: the one the copy replaced soonest after it.
func asOfJobs(jobs, versions []restoreJob, asOf time.Time) []restoreJob {
	at := map[string]restoreJob{}
	for _, v := range versions {
		if !v.stamp.After(asOf) {
			continue
		}
		key := strings.ToLower(v.path)
		if kept, ok := at[key]; !ok || v.stamp.Before(kept.stamp) {
			at[key] = v
		}
	}
	out := make([]restoreJob, len(jobs))
	for i, job := range jobs {
		out[i] = job
		if v, ok := at[strings.ToLower(job.path)]; ok {
			out[i] = v
		}
	}
	return out
}

func encryptedJobs(src Target, key *KeyFile) ([]restoreJob, []restoreJob, error) {
	var newest manifest
	found := false
	var last error
	for _, name := range manifestNames {
		sealed, err := readAll(src, name)
		if err != nil {
			last = err
			continue
		}
		m, err := openManifest(key.Key, sealed)
		if err != nil {
			last = err
			continue
		}
		if !found || m.Seq > newest.Seq {
			newest, found = m, true
		}
	}
	if !found {
		return nil, nil, fmt.Errorf("the copy's manifest can't be read with this key: %v", last)
	}
	if newest.User != key.Key.User {
		return nil, nil, fmt.Errorf("the copy is %s's, the key %s's", newest.User, key.Key.User)
	}
	var jobs, versions []restoreJob
	for _, f := range newest.Files {
		job := restoreJob{path: f.Path, blob: f.Name, size: f.Size, sha256: f.Sha256, modTime: f.ModTime}
		if stamp, name, ok := ParseHistoryPath(f.Path); ok {
			job.path, job.stamp = name, stamp
			versions = append(versions, job)
			continue
		}
		jobs = append(jobs, job)
	}
	return jobs, versions, nil
}

// run brings one file back, trying it again after a failure; it reports whether the file was there already.
func (j restoreJob) run(ctx context.Context, src Target, key *KeyFile, folder string) (bool, int64, error) {
	dest, err := localPath(folder, j.path)
	if err != nil {
		return false, 0, err
	}
	if j.there(dest) {
		return true, 0, nil
	}
	var last error
	for try := range max(1, RestoreTries) {
		if try > 0 {
			select {
			case <-ctx.Done():
				return false, 0, ctx.Err()
			case <-time.After(time.Duration(try) * 2 * time.Second):
			}
		}
		n, err := j.fetch(src, key, dest)
		if err == nil {
			return false, n, nil
		}
		last = err
	}
	return false, 0, last
}

// there reports whether the file is in place already: its size, and its sha256 when known.
func (j restoreJob) there(dest string) bool {
	info, err := os.Stat(dest)
	if err != nil || (j.size >= 0 && info.Size() != j.size) {
		return false
	}
	if j.sha256 == "" {
		return j.plainOriginalOK(dest)
	}
	sum, _, err := hashLocal(dest)
	return err == nil && sum == j.sha256
}

// plainOriginalOK checks a plain copy's original against its sidecar's sha256, when it has one beside it.
func (j restoreJob) plainOriginalOK(dest string) bool {
	if strings.HasSuffix(dest, ".json") {
		return true
	}
	want, err := sidecarShaOf(dest + ".json")
	if err != nil {
		return true // no sidecar (yet): the size is what can be checked
	}
	sum, _, err := hashLocal(dest)
	return err == nil && sum == want
}

// fetch writes the file to a temp file beside dest, checks it and renames it into place.
func (j restoreJob) fetch(src Target, key *KeyFile, dest string) (int64, error) {
	name := j.path
	switch {
	case j.blob != "":
		name = j.blob
	case j.from != "":
		name = j.from
	}
	r, err := src.Open(name)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	var body io.Reader = r
	if j.blob != "" {
		if body, err = age.Decrypt(r, key.Identity); err != nil {
			return 0, fmt.Errorf("it doesn't decrypt with this key: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".*.restoring")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), body)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	if j.sha256 != "" && (sum != j.sha256 || n != j.size) {
		return 0, fmt.Errorf("%w: it came back with sha256 %s, not %s", errDamaged, sum, j.sha256)
	}
	if j.blob == "" && !strings.HasSuffix(dest, ".json") {
		// A plain copy's original is checked against its sidecar, which may come before or after it: the one that
		// comes second checks.
		if want, err := sidecarShaOf(dest + ".json"); err == nil && want != sum {
			return 0, fmt.Errorf("%w: its bytes have sha256 %s, its sidecar names %s", errDamaged, sum, want)
		}
	}
	if err := media.Rename(tmp.Name(), dest); err != nil {
		return 0, err
	}
	if !j.modTime.IsZero() {
		os.Chtimes(dest, j.modTime, j.modTime)
	}
	if j.blob == "" && strings.HasSuffix(dest, ".json") {
		original := strings.TrimSuffix(dest, ".json")
		if _, err := os.Stat(original); err == nil && !j.plainOriginalOK(original) {
			os.Remove(original)
			return n, fmt.Errorf("%w: %s doesn't match its sidecar; run restore again to fetch it again", errDamaged, filepath.Base(original))
		}
	}
	return n, nil
}

// localPath is rel inside folder, refusing a path that leaves it.
func localPath(folder, rel string) (string, error) {
	local, err := filepath.Localize(rel)
	if err != nil || !filepath.IsLocal(local) {
		return "", fmt.Errorf("%q isn't a path in a data folder", rel)
	}
	return filepath.Join(folder, local), nil
}

func sidecarShaOf(file string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	var sidecar struct {
		Sha256 string `json:"sha256"`
	}
	if err := json.Unmarshal(data, &sidecar); err != nil || sidecar.Sha256 == "" {
		return "", fmt.Errorf("%s names no sha256", file)
	}
	return sidecar.Sha256, nil
}

func hashLocal(file string) (string, int64, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, f)
	return hex.EncodeToString(hash.Sum(nil)), n, err
}

// KeyFromPassphrase is the key file a passphrase gives for the encrypted copy in src: its derivation (salt and
// parameters) is beside the copy. A wrong passphrase gives a key that doesn't open the manifest, which Restore says.
func KeyFromPassphrase(src Target, user, passphrase string) (KeyFile, error) {
	data, err := readAll(src, derivationName)
	if err != nil {
		return KeyFile{}, fmt.Errorf("this copy's key doesn't come from a passphrase (no %s): give the key file with --key: %w", derivationName, err)
	}
	var stored struct {
		V          int        `json:"v"`
		User       string     `json:"user"`
		Derivation Derivation `json:"derivation"`
	}
	if err := json.Unmarshal(data, &stored); err != nil || stored.V != 1 {
		return KeyFile{}, fmt.Errorf("%s isn't readable: %v", derivationName, err)
	}
	if stored.User != user {
		return KeyFile{}, fmt.Errorf("the copy is %s's, not %s's", stored.User, user)
	}
	key, identity, err := DeriveKey(user, passphrase, stored.Derivation, time.Now())
	if err != nil {
		return KeyFile{}, err
	}
	return KeyFile{Key: key, Identity: identity}, nil
}
