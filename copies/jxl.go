package copies

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/journal/server-go/media"
)

// A JPEG kept as JPEG XL in the data folder (docs/media.md, "JPEG XL") leaves the copy too, once the copy has the
// JPEG XL (the owner's choice, Oct 28, 2026): the one thing a copy deletes. Only when it really is the same file:
// the copy's JPEG has the sha256 the JPEG XL's encodedFrom names, and the copy's JPEG XL, decoded with libjxl, makes
// that JPEG byte for byte. Without libjxl, or when either check fails, the JPEG stays.

// dropJPEG deletes from the copy the JPEG that item, a JPEG XL already in the copy, was kept from, and its sidecar;
// it returns the bytes freed (0 when the copy has no such JPEG). names caches the copy's month folders' file names for
// one pass.
func (c *Copier) dropJPEG(item media.Item, names map[string][]string) (int64, error) {
	from := item.Sidecar.Facts.EncodedFrom
	c.mu.Lock()
	done := c.dropped[from.Sha256]
	c.mu.Unlock()
	if done {
		return 0, nil
	}
	rel, err := c.findJPEG(item, names)
	if err != nil || rel == "" {
		if err == nil {
			c.markDropped(from.Sha256)
		}
		return 0, err
	}
	if c.store.JXL == nil {
		return 0, fmt.Errorf("%s stays in the copy beside its JPEG XL: %w", rel, media.ErrNoJXL)
	}
	jpegPath := c.target(rel)
	if sum, size, err := hashFile(jpegPath); err != nil {
		return 0, err
	} else if sum != from.Sha256 || size != from.Size {
		return 0, fmt.Errorf("%s stays in the copy: its bytes aren't the JPEG %s was kept from", rel, item.Path)
	}
	jxl, err := os.ReadFile(c.target(item.Path))
	if err != nil {
		return 0, err
	}
	if sum := sha256.Sum256(jxl); hex.EncodeToString(sum[:]) != item.Sha256() {
		return 0, fmt.Errorf("%s stays in the copy: the copy's %s isn't the JPEG XL", rel, item.Path)
	}
	jpeg, err := c.store.JXL.ToJPEG(jxl)
	if err != nil {
		return 0, fmt.Errorf("%s stays in the copy: the copy's JPEG XL doesn't make it: %w", rel, err)
	}
	if sum := sha256.Sum256(jpeg); hex.EncodeToString(sum[:]) != from.Sha256 || int64(len(jpeg)) != from.Size {
		return 0, fmt.Errorf("%s stays in the copy: the copy's JPEG XL doesn't make it byte for byte", rel)
	}
	// Its sidecar first, as the data folder does: what a stop leaves is a file no sidecar names.
	if err := os.Remove(jpegPath + ".json"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	if err := os.Remove(jpegPath); err != nil {
		return 0, err
	}
	c.markDropped(from.Sha256)
	return from.Size, nil
}

func (c *Copier) markDropped(sha string) {
	c.mu.Lock()
	c.dropped[sha] = true
	c.mu.Unlock()
}

// copySuffix is the " (2)" a name gets when the folder has it already.
var copySuffix = regexp.MustCompile(` \(\d+\)$`)

// findJPEG is where in the copy the JPEG item was kept from is (relative, with forward slashes), or "": in the JPEG
// XL's month folder, named as it was (the JPEG XL took its name, .jxl), told by its sidecar's sha256.
func (c *Copier) findJPEG(item media.Item, names map[string][]string) (string, error) {
	dir := path.Dir(item.Path)
	listed, ok := names[dir]
	if !ok {
		entries, err := os.ReadDir(c.target(dir))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		for _, e := range entries {
			if !e.IsDir() {
				listed = append(listed, e.Name())
			}
		}
		names[dir] = listed
	}
	stem := strings.ToLower(copySuffix.ReplaceAllString(strings.TrimSuffix(path.Base(item.Path), path.Ext(item.Path)), ""))
	for _, name := range listed {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, stem) || !media.IsJPEG(name, "") {
			continue
		}
		rel := dir + "/" + name
		if sha, err := sidecarSha(c.target(rel) + ".json"); err == nil && sha == item.Sidecar.Facts.EncodedFrom.Sha256 {
			return rel, nil
		}
	}
	return "", nil
}

// sidecarSha is the sha256 a sidecar names.
func sidecarSha(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var sidecar struct {
		Sha256 string `json:"sha256"`
	}
	if err := json.Unmarshal(data, &sidecar); err != nil {
		return "", err
	}
	return sidecar.Sha256, nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}
