package copies

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFolderTarget(t *testing.T) { testTarget(t, Folder(t.TempDir())) }

// testTarget is what every Target must do; a peer's target runs it too.
func testTarget(t *testing.T, target Target) {
	t.Helper()
	if err := target.Ready(); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Stat("media/2026/2026-10/a.jpg"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing file: %v", err)
	}
	if files, err := target.List("media/2026/2026-10"); err != nil || len(files) != 0 {
		t.Fatalf("a missing folder lists %v, %v", files, err)
	}
	if err := target.Walk("media", func(File) error { return errors.New("called") }); err != nil {
		t.Fatalf("walking a missing folder: %v", err)
	}
	if free, err := target.Free("media/2026/2026-10/a.jpg"); err != nil || free == 0 {
		t.Fatalf("free space beside a folder not made yet: %d, %v", free, err)
	}

	data := "the photo's bytes"
	sum := sha256.Sum256([]byte(data))
	when := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	// Bytes that aren't the ones named leave nothing behind.
	err := target.Put("media/2026/2026-10/a.jpg", strings.NewReader("other bytes"), int64(len(data)), hex.EncodeToString(sum[:]), when)
	if !errors.Is(err, errDamaged) {
		t.Fatalf("damaged bytes: %v", err)
	}
	if _, err := target.Stat("media/2026/2026-10/a.jpg"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a damaged put left a file: %v", err)
	}
	if err := target.Put("media/2026/2026-10/a.jpg", strings.NewReader(data), int64(len(data)), hex.EncodeToString(sum[:]), when); err != nil {
		t.Fatal(err)
	}
	f, err := target.Stat("media/2026/2026-10/a.jpg")
	if err != nil || f.Size != int64(len(data)) || !f.ModTime.Equal(when) {
		t.Fatalf("after a put: %+v, %v", f, err)
	}
	files, err := target.List("media/2026/2026-10")
	if err != nil || len(files) != 1 || files[0].Path != "media/2026/2026-10/a.jpg" {
		t.Fatalf("listed %+v, %v (no temp files)", files, err)
	}
	var walked []string
	if err := target.Walk("media", func(f File) error { walked = append(walked, f.Path); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(walked) != 1 || walked[0] != "media/2026/2026-10/a.jpg" {
		t.Fatalf("walked %v", walked)
	}
	if got, err := readAll(target, "media/2026/2026-10/a.jpg"); err != nil || string(got) != data {
		t.Fatalf("read %q, %v", got, err)
	}
	later := when.Add(time.Hour)
	if err := target.Touch("media/2026/2026-10/a.jpg", later); err != nil {
		t.Fatal(err)
	}
	if f, _ := target.Stat("media/2026/2026-10/a.jpg"); !f.ModTime.Equal(later) {
		t.Fatalf("touched: %v", f.ModTime)
	}
	if err := target.Remove("media/2026/2026-10/a.jpg"); err != nil {
		t.Fatal(err)
	}
	if err := target.Remove("media/2026/2026-10/a.jpg"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removing it again: %v", err)
	}
}

func TestFolderTargetOutOfReach(t *testing.T) {
	if err := Folder(filepath.Join(t.TempDir(), "unplugged")).Ready(); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("a missing folder: %v", err)
	}
}
