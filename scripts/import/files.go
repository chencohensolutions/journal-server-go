package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// File and path helpers.

// pathInside is where a file goes inside the imported folder: its path from the searched folder it was
// found in, under that folder's name (D:\Archive\phone DCIM\Camera\x.jpg → phone DCIM\Camera\x.jpg; a
// whole drive searched, E:\, gives E\…).
func pathInside(roots []string, path string) string {
	for _, root := range roots {
		if !within(path, root) {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			break
		}
		name := filepath.Base(root)
		if name == "." || strings.ContainsAny(name, `\/:`) {
			name = strings.Trim(filepath.VolumeName(root), `:\/`)
		}
		return filepath.Join(name, rel)
	}
	return filepath.Base(path)
}

// moveFile moves from to to, or to "name (2).ext", … when to is taken. When it can't be renamed there
// (another disk), it is copied, then deleted.
func moveFile(from, to string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return "", err
	}
	stem, ext := strings.TrimSuffix(to, filepath.Ext(to)), filepath.Ext(to)
	for n := 2; ; n++ {
		if _, err := os.Lstat(to); errors.Is(err, os.ErrNotExist) {
			break
		}
		to = fmt.Sprintf("%s (%d)%s", stem, n, ext)
	}
	var err error
	for attempt := range 5 {
		// On Windows a virus scanner or the search indexer briefly holding the file makes a rename fail.
		if err = os.Rename(from, to); err == nil {
			return to, nil
		}
		time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
	}
	if copyErr := copyFile(from, to); copyErr != nil {
		os.Remove(to)
		return "", fmt.Errorf("%v; copying: %v", err, copyErr)
	}
	return to, os.Remove(from)
}

func copyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

// within reports whether path is dir or inside it, ignoring case (the folders must also work on Windows and macOS).
func within(path, dir string) bool {
	rel, err := filepath.Rel(strings.ToLower(dir), strings.ToLower(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func absolute(path string) string {
	abs, err := filepath.Abs(path)
	check(err)
	return abs
}

// timeLeft estimates the time left from the speed so far: "?" for the first 10 seconds.
func timeLeft(elapsed time.Duration, done, total int64) string {
	if done <= 0 || elapsed < 10*time.Second {
		return "?"
	}
	rest := time.Duration(float64(elapsed) * float64(total-done) / float64(done))
	return rest.Round(time.Minute).String()
}

func gb(bytes int64) string { return fmt.Sprintf("%.1f GB", float64(bytes)/(1<<30)) }
