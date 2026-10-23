package ocr

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestWordsMatchAsPeopleSearch(t *testing.T) {
	for _, c := range []struct {
		word, query string
		want        bool
	}{
		{"קבלה", "קבלה", true},
		{"בקבלה", "קבלה", true},    // a prefix letter
		{"ושהקבלה", "קבלה", false}, // three are too many
		{"מספרים", "מספר", true},   // the start of a word
		{"invoice", "inv", true},
		{"invoice", "in", false},
		{"שלום", "שלי", false},
		{"קבלהx", "קבלה", true},
	} {
		w, q := tokenize(c.word)[0], tokenize(c.query)[0]
		if got := matches(w, q); got != c.want {
			t.Errorf("%q answers %q: %v, want %v", c.word, c.query, got, c.want)
		}
	}
	// Final letters and niqqud don't count; case doesn't either.
	if got := tokenize("שָׁלוֹם, Hello!"); !slices.Equal(got, []string{"שלומ", "hello"}) {
		t.Errorf("tokenize: %q", got)
	}
}

func TestTextsAreKeptAndFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.jsonl")
	x, err := openTexts(path)
	if err != nil {
		t.Fatal(err)
	}
	a, b, none := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	x.add(a, []Line{{Text: "קבלה מספר 4521"}, {Text: "Invoice total 349.90"}})
	x.add(b, []Line{{Text: "תל אביב יפו"}, {Text: "בקבלה"}})
	x.add(none, nil)
	x.close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"sha256":"cut sh`)
	f.Close()

	x, err = openTexts(path)
	if err != nil {
		t.Fatal(err)
	}
	defer x.close()
	if !x.has(none) || len(x.lines(none)) != 0 || len(x.lines(a)) != 2 {
		t.Fatal("not kept across a restart")
	}
	all := func(string) bool { return true }
	if got := x.find("קבלה", all); !slices.Equal(got, []string{a, b}) && !slices.Equal(got, []string{b, a}) {
		t.Errorf("קבלה: %v", got)
	}
	if got := x.find("invoice 349", all); !slices.Equal(got, []string{a}) {
		t.Errorf("invoice 349: %v", got)
	}
	if got := x.find("תל אביב", all); !slices.Equal(got, []string{b}) {
		t.Errorf("תל אביב: %v", got)
	}
	if got := x.find("קבלה", func(sha string) bool { return sha != b }); !slices.Equal(got, []string{a}) {
		t.Errorf("kept: %v", got)
	}
	if got := x.find("nothing here", all); len(got) != 0 {
		t.Errorf("nothing: %v", got)
	}
}

// A server whose indexes another process writes reads the texts without touching the file; refresh picks up whole
// lines and leaves one still being written.
func TestTextsReadFromAnotherProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.jsonl")
	reader, err := openTextsReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := openTexts(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.close()
	a := strings.Repeat("a", 64)
	writer.add(a, []Line{{Text: "קבלה"}})
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"sha256":"` + strings.Repeat("b", 64) + `","li`)
	f.Close()
	if n, err := reader.refresh(); err != nil || n != 1 || !reader.has(a) || len(reader.find("קבלה", func(string) bool { return true })) != 1 {
		t.Fatalf("refresh: %d, %v", n, err)
	}
	if err := reader.add(a, nil); err == nil {
		t.Error("wrote into a file another process writes")
	}
}
