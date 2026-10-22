package media

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FailureLog lists the files a background reader (text search, faces) could not read, in a file under the data
// folder's .cache: one line each, appended, with when (UTC), the sha256, the file's path in the data folder and why,
// separated by tabs. Every failure is listed, also those not logged one by one (without libvips or ffmpeg every file
// fails alike). The file is opened at the first failure; when it can't be written, that is logged once and the
// failures are only logged as before. Close it when the run ends.
type FailureLog struct {
	Path   string
	prefix string // of what is logged: "search", "faces"
	file   *os.File
	err    error
}

// NewFailureLog is the list .cache/<folder>/<name> of store, for the reader prefix.
func NewFailureLog(store *Store, prefix, folder, name string) *FailureLog {
	return &FailureLog{Path: filepath.Join(store.Root(), ".cache", folder, name), prefix: prefix}
}

// Add lists item, which could not be read for err.
func (l *FailureLog) Add(item Item, err error) {
	if l.file == nil && l.err == nil {
		if l.file, l.err = os.OpenFile(l.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); l.err != nil {
			log.Printf("%s: can't list failures in %s: %v", l.prefix, l.Path, l.err)
		}
	}
	if l.file != nil {
		reason := strings.Join(strings.Fields(err.Error()), " ") // one line
		fmt.Fprintf(l.file, "%s\t%s\t%s\t%s\n", time.Now().UTC().Format(time.RFC3339), item.Sha256(), item.Path, reason)
	}
}

// Close closes the file, if a failure opened it.
func (l *FailureLog) Close() {
	if l.file != nil {
		l.file.Close()
	}
}
