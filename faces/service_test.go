package faces

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/media"
)

// Every file that can't be read is listed in .cache/faces/<model>.failed.log, also when the reason (no libvips) is
// not logged file by file, and it is tried again only after a restart.
func TestFilesThatCantBeReadAreListed(t *testing.T) {
	f := newFixture(t)
	f.photo()
	f.photo()
	// Two files whose faces haven't been read, and can't be: there is no libvips.
	c, d := f.file(), f.file()
	library, err := media.OpenLibrary(map[string]string{"hen": f.store.Root()}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	f.index.Close() // the service opens its own
	s, err := NewService(&Model{Name: "test"}, library, media.NewPreviews("", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	failed := map[string]bool{}
	store := library.Store("hen")
	s.readNew(context.Background(), "hen", s.users["hen"], failed)
	s.readNew(context.Background(), "hen", s.users["hen"], failed)

	data, err := os.ReadFile(filepath.Join(store.Root(), ".cache", "faces", "test.failed.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("listed:\n%s", data)
	}
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		item, ok := store.Get(fields[1])
		if len(fields) != 4 || !ok || (fields[1] != c && fields[1] != d) || fields[2] != item.Path || fields[3] == "" {
			t.Errorf("line %q", line)
		}
		if _, err := time.Parse(time.RFC3339, fields[0]); err != nil {
			t.Errorf("line %q: %v", line, err)
		}
	}
}
