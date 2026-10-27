package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/journal/server-go/media"
)

// A journal is what one real run changed, so it can be undone (--undo): relabels/<UTC time>.tsv at the data
// folder's root, not under .cache, which is what can be rebuilt and what the copies leave out. After two comment
// lines and a header, one line per change, tab-separated:
//
//	<sha256>	source	<old>	<new>
//	<sha256>	screenshotApp	<old>	<new>
//	<sha256>	key	<old>	<new>                 the key the screenshot's app is written under: app or screenshotApp
//	settings.json	hidden.sources	<old id>	<new id>
//
// An empty value is none. A tab, a newline or a backslash in a value is written \t, \n, \\. Each line is written
// and synced before the change it records, so a run stopped midway leaves a journal of everything it changed (and at
// most one line for a change it didn't make, which undo then finds not done and skips).
type journal struct {
	root, user, mode string
	now              func() time.Time
	f                *os.File
	path             string
}

// journalLine is one change in a journal.
type journalLine struct {
	sha, field, old, new string
}

const (
	fieldSource        = "source"
	fieldScreenshotApp = "screenshotApp"
	fieldKey           = "key"
	fieldHiddenSources = "hidden.sources"
	settingsFile       = "settings.json"
	journalHeader      = "sha256\tfield\told\tnew"
)

// write appends lines to the journal, making it at its first line.
func (j *journal) write(lines ...journalLine) error {
	if len(lines) == 0 {
		return nil
	}
	if j.f == nil {
		if err := j.create(); err != nil {
			return err
		}
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(strings.Join([]string{escape(l.sha), escape(l.field), escape(l.old), escape(l.new)}, "\t") + "\n")
	}
	if _, err := j.f.WriteString(b.String()); err != nil {
		return err
	}
	return j.f.Sync()
}

func (j *journal) create() error {
	dir := filepath.Join(j.root, "relabels")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	at := j.now().UTC()
	base := at.Format("2006-01-02T150405Z")
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
	head := fmt.Sprintf("# fix-sources %s for %s at %s\n# undo with: go run ./scripts/fix-sources --user %s --undo \"%s\"\n%s\n",
		j.mode, j.user, at.Format(time.RFC3339), j.user, j.path, journalHeader)
	if _, err := j.f.WriteString(head); err != nil {
		return err
	}
	return j.f.Sync()
}

func (j *journal) close() error {
	if j.f == nil {
		return nil
	}
	return j.f.Close()
}

// labelLines are the journal's lines for a file whose labels go from old to new.
func labelLines(sha string, old, new media.Labels) []journalLine {
	var lines []journalLine
	if old.Source != new.Source {
		lines = append(lines, journalLine{sha, fieldSource, old.Source, new.Source})
	}
	if old.ScreenshotApp != new.ScreenshotApp {
		lines = append(lines, journalLine{sha, fieldScreenshotApp, old.ScreenshotApp, new.ScreenshotApp})
	}
	if a, b := keyOf(old), keyOf(new); a != b {
		lines = append(lines, journalLine{sha, fieldKey, a, b})
	}
	return lines
}

// keyOf is the key a screenshot's app is written under, "" without one.
func keyOf(l media.Labels) string {
	switch {
	case l.ScreenshotApp == "":
		return ""
	case l.OldAppKey:
		return "app"
	}
	return "screenshotApp"
}

// readJournal reads a journal's changes, in the order they were made.
func readJournal(path string) ([]journalLine, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []journalLine
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for n := 1; scanner.Scan(); n++ {
		text := strings.TrimRight(scanner.Text(), "\r")
		if text == "" || strings.HasPrefix(text, "#") || text == journalHeader {
			continue
		}
		parts := strings.Split(text, "\t")
		if len(parts) != 4 {
			return nil, fmt.Errorf("%s:%d: %d fields, not 4", path, n, len(parts))
		}
		l := journalLine{unescape(parts[0]), unescape(parts[1]), unescape(parts[2]), unescape(parts[3])}
		switch {
		case l.sha == settingsFile && l.field == fieldHiddenSources:
		case media.IsSha256(l.sha) && (l.field == fieldSource || l.field == fieldScreenshotApp || l.field == fieldKey):
		default:
			return nil, fmt.Errorf("%s:%d: not a change this tool makes: %q", path, n, text)
		}
		lines = append(lines, l)
	}
	return lines, scanner.Err()
}

var (
	escaper   = strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`)
	unescaper = strings.NewReplacer(`\\`, `\`, `\t`, "\t", `\n`, "\n", `\r`, "\r")
)

func escape(s string) string   { return escaper.Replace(s) }
func unescape(s string) string { return unescaper.Replace(s) }
