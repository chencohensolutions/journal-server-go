package importcmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/journal/server-go/media"
)

// sourceHashes is the sha256 of every file looked at in the folders being imported, kept in the data
// folder's .cache/source-hashes.tsv ("<path>\t<size>\t<modified, Unix ns>\t<sha256>") so a dry run's hashing
// isn't done again by the import that follows, nor a run stopped halfway by the run that goes on. A file the import
// moves (to the imported or not-imported folder) is added where it went, so importing that folder later reads none
// of it again. A file whose size or modified time changed is hashed again. Each line is written as soon as its file
// is hashed. It is only a cache: deleting it costs a slower next run.
type sourceHashes struct {
	mu     sync.Mutex
	hashes map[string]sourceHash
	file   *os.File
}

type sourceHash struct {
	size     int64
	modified int64
	sha256   string
}

func openSourceHashes(dataFolder string) (*sourceHashes, error) {
	path := filepath.Join(dataFolder, ".cache", "source-hashes.tsv")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	t := &sourceHashes{hashes: map[string]sourceHash{}}
	if f, err := os.Open(path); err == nil {
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			fields := strings.Split(scanner.Text(), "\t")
			if len(fields) != 4 || len(fields[3]) != 64 {
				continue
			}
			size, errS := strconv.ParseInt(fields[1], 10, 64)
			modified, errM := strconv.ParseInt(fields[2], 10, 64)
			if errS == nil && errM == nil {
				t.hashes[fields[0]] = sourceHash{size, modified, fields[3]}
			}
		}
		f.Close()
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	t.file = file
	return t, nil
}

// sha256 returns the file's sha256 from the table when its size and modified time are unchanged, or
// hashes it and adds it.
func (t *sourceHashes) sha256(path string, info os.FileInfo) (string, error) {
	modified := info.ModTime().UnixNano()
	t.mu.Lock()
	known, ok := t.hashes[path]
	t.mu.Unlock()
	if ok && known.size == info.Size() && known.modified == modified {
		return known.sha256, nil
	}
	sha, err := media.HashFile(path)
	if err != nil {
		return "", err
	}
	t.add(path, info.Size(), modified, sha)
	return sha, nil
}

func (t *sourceHashes) add(path string, size, modified int64, sha string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.hashes[path] = sourceHash{size, modified, sha}
	fmt.Fprintf(t.file, "%s\t%d\t%d\t%s\n", path, size, modified, sha)
}

// moved notes that the file at from, whose sha256 is known, is now at to: the same bytes (a move, or a copy and
// delete to another disk, which may give it another modified time).
func (t *sourceHashes) moved(from, to string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	known, ok := t.hashes[from]
	t.mu.Unlock()
	if !ok {
		return
	}
	if info, err := os.Stat(to); err == nil && info.Size() == known.size {
		t.add(to, info.Size(), info.ModTime().UnixNano(), known.sha256)
	}
}

func (t *sourceHashes) close() error { return t.file.Close() }

// each calls fn with every path in the table and its sha256.
func (t *sourceHashes) each(fn func(path, sha string)) {
	t.mu.Lock()
	hashes := make(map[string]string, len(t.hashes))
	for path, h := range t.hashes {
		hashes[path] = h.sha256
	}
	t.mu.Unlock()
	for path, sha := range hashes {
		fn(path, sha)
	}
}
