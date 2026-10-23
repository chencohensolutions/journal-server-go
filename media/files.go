package media

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// HashFile is the sha256 of the file's content, as 64 lowercase hex characters.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, bufio.NewReaderSize(f, 1<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeFileAtomic writes through a temp file in the same folder and a rename, so a crash never
// leaves a half-written file behind.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return rename(tmp.Name(), path)
}

// rename retries on Windows, where a virus scanner or the search indexer briefly holding a file
// open makes a rename fail.
func rename(from, to string) error {
	var err error
	for attempt := range 10 {
		if err = os.Rename(from, to); err == nil || runtime.GOOS != "windows" {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
	}
	return err
}

// Rename is rename for other packages (copies): it retries on Windows while another program holds the file.
func Rename(from, to string) error { return rename(from, to) }

// FreeBytes is the space available to this process on the disk holding path; unknown platforms report no limit.
func FreeBytes(path string) (uint64, error) { return freeBytes(path) }
