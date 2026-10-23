package ocr

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/journal/server-go/media"
	"github.com/journal/server-go/reading"
)

// Service is the text in the users' photos: an index of package reading, and the words text search finds.
type Service struct {
	model   *Model
	library *media.Library
	users   map[string]*texts
}

// NewService opens the model's texts in the data folder of each of library's users.
func NewService(model *Model, library *media.Library) (*Service, error) {
	s := &Service{model: model, library: library, users: map[string]*texts{}}
	for _, user := range library.Users() {
		t, err := openTexts(filepath.Join(library.Store(user).Root(), ".cache", "ocr", model.Name+".jsonl"))
		if err != nil {
			return nil, fmt.Errorf("user %s: %w", user, err)
		}
		s.users[user] = t
	}
	return s, nil
}

// NewReadOnlyService answers queries about the text another process reads (journal-server index), named after
// its models: each user's texts opened to read only. Refresh reads what that process added.
func NewReadOnlyService(model string, library *media.Library) (*Service, error) {
	s := &Service{model: &Model{Name: model}, library: library, users: map[string]*texts{}}
	for _, user := range library.Users() {
		t, err := openTextsReadOnly(filepath.Join(library.Store(user).Root(), ".cache", "ocr", model+".jsonl"))
		if err != nil {
			return nil, fmt.Errorf("user %s: %w", user, err)
		}
		s.users[user] = t
	}
	return s, nil
}

// Refresh reads the text another process read since the last look (NewReadOnlyService).
func (s *Service) Refresh() error {
	for user, t := range s.users {
		if _, err := t.refresh(); err != nil {
			return fmt.Errorf("user %s: %w", user, err)
		}
	}
	return nil
}

// Model is the name of the models.
func (s *Service) Model() string { return s.model.Name }

func (s *Service) Name() string { return "ocr" }

// Order: after the nudity scan and faces, before text search.
func (s *Service) Order() int { return 15 }

func (s *Service) Wants() reading.Wants {
	return reading.Wants{Kinds: []media.Kind{media.KindPhoto}, Pictures: []media.PreviewSpec{Picture}, Regions: true}
}

func (s *Service) Needs(user string, item media.Item) bool {
	t := s.users[user]
	return t != nil && !t.has(item.Sha256())
}

// Read finds the file's text and keeps it (none is kept too: the file has been read).
func (s *Service) Read(ctx context.Context, user string, in reading.Input) error {
	var region Region
	if in.Original != nil {
		region = in.Original.Region
	}
	lines, err := s.model.Read(in.Pictures[Picture], region)
	if err != nil {
		return err
	}
	return s.users[user].add(in.Item.Sha256(), lines)
}

func (s *Service) Finish(user string) {}

func (s *Service) FailureLog() (folder, name string) { return "ocr", s.model.Name + ".failed.log" }

// Text is the lines of text found in user's file sha, top to bottom; none when it has none or hasn't been read.
func (s *Service) Text(user, sha string) []Line {
	if t := s.users[user]; t != nil {
		return t.lines(sha)
	}
	return nil
}

// Find is user's files whose text has every word of query, those with the most matches first, of those keep
// accepts. A word matches the same word, one it begins (of three letters or more), and in Hebrew one with up to two
// of the letters that are written onto a word before it (ה ו ב כ ל מ ש: "קבלה" finds "בקבלה"). Niqqud, the final
// letters' shapes and letter case don't count.
func (s *Service) Find(user, query string, keep func(sha string) bool) []string {
	if t := s.users[user]; t != nil {
		return t.find(query, keep)
	}
	return nil
}

// CacheFile is user's texts file; "" for a user with no data folder.
func (s *Service) CacheFile(user string) string {
	if t := s.users[user]; t != nil {
		return t.path
	}
	return ""
}

// Has reports whether user's photo sha has been read for text.
func (s *Service) Has(user, sha string) bool {
	t := s.users[user]
	return t != nil && t.has(sha)
}

// Record is user's photo sha's line in the texts file, without its newline; nil when it hasn't been read.
func (s *Service) Record(user, sha string) []byte {
	t := s.users[user]
	if t == nil || !t.has(sha) {
		return nil
	}
	data, _ := json.Marshal(record{sha, t.lines(sha)})
	return data
}

// Counts is how many of user's photos have been read for text, of all of them; files hide reports true for are left
// out of both.
func (s *Service) Counts(user string, hide func(media.Item) bool) (read, total int) {
	t, store := s.users[user], s.library.Store(user)
	if t == nil {
		return 0, 0
	}
	store.Range(func(item media.Item) {
		if hide(item) || item.Sidecar.Facts.Kind != media.KindPhoto {
			return
		}
		total++
		if t.has(item.Sha256()) {
			read++
		}
	})
	return read, total
}

// Close closes the users' text files.
func (s *Service) Close() {
	for _, t := range s.users {
		t.close()
	}
}

// texts is the text found in one user's files, kept in .cache/ocr/<model>.jsonl: one JSON line a file read,
// {"sha256": …, "lines": [{"text", "box", "score"}, …]} (no lines: read, none found), appended as files are read; a
// line cut short by a stop is dropped when the file is opened, and a file read again has its last line counted.
// Only one process writes it: the server, or `journal-server index` (config.json "index": "daily"), while the
// server reads it (openTextsReadOnly, refresh). It can be deleted: everything is read again.
type texts struct {
	path string
	// Opened to read only: never written or cut, and read on from end by refresh.
	readOnly bool
	end      int64

	mu    sync.RWMutex
	file  *os.File
	files map[string][]Line         // by sha256
	words map[string]map[string]int // word (normalized) → sha256 → how often
}

type record struct {
	Sha256 string `json:"sha256"`
	Lines  []Line `json:"lines"`
}

func openTexts(path string) (*texts, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	t := &texts{path: path, file: f, files: map[string][]Line{}, words: map[string]map[string]int{}}
	end, _ := t.records(f, 0)
	if err := f.Truncate(end); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(end, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return t, nil
}

// records reads whole lines from r, which is at end, and returns where the last whole one ends and how many there
// were: a line without its newline (being written, or cut short by a stop) is left.
func (t *texts) records(r io.Reader, end int64) (int64, int) {
	br := bufio.NewReaderSize(r, 1<<20)
	n := 0
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return end, n
		}
		var rec record
		if json.Unmarshal(bytes.TrimSpace(line), &rec) == nil && media.IsSha256(rec.Sha256) {
			t.put(rec.Sha256, rec.Lines)
			n++
		}
		end += int64(len(line))
	}
}

// openTextsReadOnly opens the texts at path to read only, for a server whose indexes another process writes
// (journal-server index): nothing is written or cut, and refresh reads on. A file not there yet is no text yet.
func openTextsReadOnly(path string) (*texts, error) {
	t := &texts{path: path, readOnly: true, files: map[string][]Line{}, words: map[string]map[string]int{}}
	if _, err := t.refresh(); err != nil {
		return nil, err
	}
	return t, nil
}

// refresh reads the lines appended since the last look and returns how many there were; a line still being
// written is left for the next look, and a file started over is read again from its start.
func (t *texts) refresh() (int, error) {
	if !t.readOnly {
		return 0, errors.New("the texts are written here, not read from another process")
	}
	f, err := os.Open(t.path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if info.Size() < t.end {
		t.files, t.words, t.end = map[string][]Line{}, map[string]map[string]int{}, 0
	}
	if _, err := f.Seek(t.end, io.SeekStart); err != nil {
		return 0, err
	}
	var n int
	t.end, n = t.records(f, t.end)
	return n, nil
}

func (t *texts) put(sha string, lines []Line) {
	if old, ok := t.files[sha]; ok {
		for _, w := range wordsOf(old) {
			delete(t.words[w], sha)
		}
	}
	if lines == nil {
		lines = []Line{}
	}
	t.files[sha] = lines
	for _, w := range wordsOf(lines) {
		if t.words[w] == nil {
			t.words[w] = map[string]int{}
		}
		t.words[w][sha]++
	}
}

func (t *texts) add(sha string, lines []Line) error {
	if t.readOnly {
		return errors.New("the texts are read from another process's file, not written here")
	}
	data, err := json.Marshal(record{sha, lines})
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, err := t.file.Write(append(data, '\n')); err != nil {
		return err
	}
	t.put(sha, lines)
	return nil
}

func (t *texts) has(sha string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.files[sha]
	return ok
}

func (t *texts) lines(sha string) []Line {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.files[sha]
}

func (t *texts) find(query string, keep func(sha string) bool) []string {
	tokens := tokenize(query)
	if len(tokens) == 0 {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	var counts map[string]int
	for _, token := range tokens {
		these := map[string]int{}
		for w, shas := range t.words {
			if !matches(w, token) {
				continue
			}
			for sha, n := range shas {
				if counts == nil || counts[sha] > 0 {
					these[sha] += n
				}
			}
		}
		for sha, n := range these {
			these[sha] = n + counts[sha]
		}
		counts = these
		if len(counts) == 0 {
			return nil
		}
	}
	var found []string
	for sha := range counts {
		if keep(sha) {
			found = append(found, sha)
		}
	}
	slices.SortFunc(found, func(a, b string) int { return cmp.Or(cmp.Compare(counts[b], counts[a]), strings.Compare(a, b)) })
	return found
}

func (t *texts) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.file == nil {
		return
	}
	if err := t.file.Close(); err != nil {
		log.Printf("ocr: %v", err)
	}
}

// wordsOf is the normalized words of lines.
func wordsOf(lines []Line) []string {
	var words []string
	for _, l := range lines {
		words = append(words, tokenize(l.Text)...)
	}
	return words
}

// tokenize splits text into words of letters and digits, normalized: lower case, no niqqud, Hebrew's final letters
// as the others.
func tokenize(text string) []string {
	var words []string
	for _, w := range strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !isNiqqud(r) }) {
		var b strings.Builder
		for _, r := range strings.ToLower(w) {
			if isNiqqud(r) {
				continue
			}
			if f, final := finals[r]; final {
				r = f
			}
			b.WriteRune(r)
		}
		if b.Len() > 0 {
			words = append(words, b.String())
		}
	}
	return words
}

// finals maps Hebrew's final letters to the same letters as written elsewhere in a word.
var finals = map[rune]rune{'ך': 'כ', 'ם': 'מ', 'ן': 'נ', 'ף': 'פ', 'ץ': 'צ'}

func isNiqqud(r rune) bool {
	return r >= 0x0591 && r <= 0x05C7 && r != 0x05BE && r != 0x05C0 && r != 0x05C3 && r != 0x05C6
}

// hebrewPrefixes are the letters written onto a Hebrew word before it: the, and, in, as, to, from, that.
const hebrewPrefixes = "הובכלמש"

// matches reports whether the word w (from a photo) answers the query word q.
func matches(w, q string) bool {
	if w == q || (len([]rune(q)) >= 3 && strings.HasPrefix(w, q)) {
		return true
	}
	rest, ok := strings.CutSuffix(w, q)
	if !ok || rest == "" {
		return false
	}
	runes := []rune(rest)
	if len(runes) > 2 || !unicode.Is(unicode.Hebrew, []rune(q)[0]) {
		return false
	}
	for _, r := range runes {
		if !strings.ContainsRune(hebrewPrefixes, r) {
			return false
		}
	}
	return true
}

// Tokenize is text's words as Find compares them: letters and digits, lower case, without niqqud, and Hebrew's
// final letters as the others. Apps that search the text themselves do the same (scripts/local-fixtures).
func Tokenize(text string) []string { return tokenize(text) }

// Matches reports whether the word w (from a photo, tokenized) answers the query word q (tokenized), as Find does.
func Matches(w, q string) bool { return matches(w, q) }

// FindIn is Find over the texts given (by sha256), for apps to check their own search against
// (scripts/local-fixtures).
func FindIn(files map[string][]Line, query string) []string {
	t := &texts{files: map[string][]Line{}, words: map[string]map[string]int{}}
	for sha, lines := range files {
		t.put(sha, lines)
	}
	return t.find(query, func(string) bool { return true })
}
