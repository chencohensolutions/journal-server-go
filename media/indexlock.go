package media

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrIndexesLocked is another process reading this data folder's files for the indexes: a server whose own
// reader does it (config.json "index": "server"), or another `journal-server index` run.
var ErrIndexesLocked = errors.New("another process is reading the files for the indexes")

// LockIndexes takes .cache/index.lock, which whoever writes the indexes' caches (.cache/faces, ocr, search,
// similar, aesthetic) holds, so that two processes never write one file at once. It doesn't wait: when another
// process holds it, it answers ErrIndexesLocked. The operating system drops the lock if the process dies.
func (s *Store) LockIndexes() (unlock func(), err error) {
	path := filepath.Join(s.root, ".cache", "index.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	locked, err := tryLockFile(f)
	if err != nil || !locked {
		f.Close()
		if err == nil {
			err = ErrIndexesLocked
		}
		return nil, err
	}
	return func() {
		unlockFile(f)
		f.Close()
	}, nil
}
