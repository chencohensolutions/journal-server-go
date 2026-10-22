package search

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/media"
)

// Every file that can't be read is listed in .cache/search/<model>.failed.log, also when the reason (no libvips) is
// not logged file by file, and it is tried again only after a restart.
func TestFilesThatCantBeReadAreListed(t *testing.T) {
	root := t.TempDir()
	library, err := media.OpenLibrary(map[string]string{"hen": root}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	store := library.Store("hen")
	var shas []string
	for i := range 2 {
		content := []byte(fmt.Sprintf("photo %d", i))
		path := filepath.Join(t.TempDir(), "photo.jpg")
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		sha := hex.EncodeToString(sum[:])
		if _, _, err := store.Import(path, media.UploadRequest{Sha256: sha, Size: int64(len(content)),
			FileName: fmt.Sprintf("IMG_%d.jpg", i), MimeType: "image/jpeg", Kind: media.KindPhoto, UploadedBy: "test"}); err != nil {
			t.Fatal(err)
		}
		shas = append(shas, sha)
	}
	s, err := NewService(&Model{Name: "test", Dim: 2}, library, media.NewPreviews("", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	failed := map[string]bool{}
	s.readNew(context.Background(), "hen", failed)
	s.readNew(context.Background(), "hen", failed)

	data, err := os.ReadFile(filepath.Join(root, ".cache", "search", "test.failed.log"))
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
		if len(fields) != 4 || !ok || fields[2] != item.Path || fields[3] == "" {
			t.Errorf("line %q", line)
		}
		if _, err := time.Parse(time.RFC3339, fields[0]); err != nil {
			t.Errorf("line %q: %v", line, err)
		}
	}
}
