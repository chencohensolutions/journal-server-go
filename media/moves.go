package media

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// backgroundMoves are the moves a user's mark started (SetCamera, SetSoundFolder): files relabelled in the
// background, a turn each, after the mark was answered at once. One runs at a time (run), and pending says how many
// files of each key (a camera model, a sound folder) are still to move.
type backgroundMoves struct {
	run     sync.Mutex
	mu      sync.Mutex
	pending map[string]int
	queued  map[string]int
	running sync.WaitGroup
}

// relabelMove is a file whose source changes with a mark.
type relabelMove struct {
	sha, source string
	app         *string
}

// queue counts a move of key's files about to start, with n files to move as the marks are now.
func (m *backgroundMoves) queue(key string, n int) {
	m.mu.Lock()
	if m.pending == nil {
		m.pending = map[string]int{}
		m.queued = map[string]int{}
	}
	m.queued[key]++
	if n > 0 || m.pending[key] > 0 {
		m.pending[key] = max(m.pending[key], n)
	}
	m.mu.Unlock()
	m.running.Add(1)
}

// pendingOf is how many files of key are still to move.
func (m *backgroundMoves) pendingOf(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pending[key]
}

// do moves key's files as moving lists them when this move's turn comes (after any move before it), each by apply,
// counting down pending; what names the move in the log.
func (m *backgroundMoves) do(key string, moving func() []relabelMove, apply func(relabelMove) error, what string) {
	defer m.running.Done()
	m.run.Lock()
	defer m.run.Unlock()
	moves := moving()
	setPending := func(n int) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if n > 0 || m.queued[key] > 1 {
			m.pending[key] = n
		} else {
			delete(m.pending, key)
		}
	}
	setPending(len(moves))
	for i, move := range moves {
		if err := apply(move); err != nil {
			log.Printf("media: moving %s to %s after %s: %v", move.sha, move.source, what, err)
		}
		setPending(len(moves) - i - 1)
	}
	m.mu.Lock()
	m.queued[key]--
	if m.queued[key] <= 0 {
		delete(m.queued, key)
		delete(m.pending, key)
	}
	m.mu.Unlock()
}

// wait waits until the moves started are done.
func (m *backgroundMoves) wait() { m.running.Wait() }

// relabelJournal is the journal of a move the server makes itself (SetSoundFolder), written as scripts/fix-sources
// writes its own (relabels/<UTC time>-<what>.tsv at the data folder's root; see docs/media.md "Fixing sources"), so
// `fix-sources --undo` undoes it: two comment lines, a header, then one tab-separated line per source changed,
// written and synced before the change it records. It is made at its first line.
type relabelJournal struct {
	root, what, about string
	now               func() time.Time
	f                 *os.File
	path              string
}

var journalEscaper = strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`)

// write appends the change of a file's source from old to new.
func (j *relabelJournal) write(sha, old, new string) error {
	if j.f == nil {
		if err := j.create(); err != nil {
			return err
		}
	}
	line := strings.Join([]string{sha, "source", journalEscaper.Replace(old), journalEscaper.Replace(new)}, "\t") + "\n"
	if _, err := j.f.WriteString(line); err != nil {
		return err
	}
	return j.f.Sync()
}

func (j *relabelJournal) create() error {
	dir := filepath.Join(j.root, "relabels")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	at := j.now().UTC()
	base := at.Format("2006-01-02T150405Z") + "-" + j.what
	for n := 1; ; n++ {
		name := base + ".tsv"
		if n > 1 {
			name = fmt.Sprintf("%s-%d.tsv", base, n)
		}
		path := filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		j.f, j.path = f, path
		break
	}
	head := fmt.Sprintf("# %s at %s\n# undo with: go run ./scripts/fix-sources --user <user> --undo \"%s\"\nsha256\tfield\told\tnew\n",
		journalEscaper.Replace(j.about), at.Format(time.RFC3339), j.path)
	if _, err := j.f.WriteString(head); err != nil {
		return err
	}
	return j.f.Sync()
}

func (j *relabelJournal) close() error {
	if j.f == nil {
		return nil
	}
	return j.f.Close()
}
