package main

import (
	"io"
	"os"
	"sync"
)

// maxLogBytes is how large the service's log (server.log) grows before its lines move to server.log.old.
const maxLogBytes = 20 << 20

// cappedLog is a log file that never grows past max: at max, what it holds is copied to <name>.old (in place of the
// one before) and the file starts again empty. The file itself stays open and is never renamed, which Windows
// refuses for an open file, so whatever else writes to the same handle (os.Stdout, os.Stderr) goes on working.
type cappedLog struct {
	mu   sync.Mutex
	file *os.File
	max  int64
	size int64
}

// openCappedLog opens (or makes) the log at path for appending.
func openCappedLog(path string, max int64) (*cappedLog, error) {
	// Not O_APPEND: Windows then opens the file for appending only, and it couldn't be emptied. Writes go where
	// the handle stands, which is the end.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		file.Close()
		return nil, err
	}
	l := &cappedLog{file: file, max: max}
	if info, err := file.Stat(); err == nil {
		l.size = info.Size()
	}
	return l, nil
}

func (l *cappedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size+int64(len(p)) > l.max {
		l.startAgain()
	}
	n, err := l.file.Write(p)
	l.size += int64(n)
	return n, err
}

// startAgain moves the lines to <name>.old and empties the file. A failure leaves the log growing: losing lines
// would be worse than a large file. Callers hold mu.
func (l *cappedLog) startAgain() {
	// What was written through the handle directly (os.Stderr) counts too.
	if info, err := l.file.Stat(); err == nil {
		l.size = info.Size()
	}
	old, err := os.Create(l.file.Name() + ".old")
	if err != nil {
		return
	}
	defer old.Close()
	// Whatever happens, the next line goes to the file's end.
	defer l.file.Seek(0, io.SeekEnd)
	if _, err := l.file.Seek(0, io.SeekStart); err != nil {
		return
	}
	if _, err := io.Copy(old, l.file); err != nil {
		return
	}
	if err := l.file.Truncate(0); err != nil {
		return
	}
	l.size = 0
}
