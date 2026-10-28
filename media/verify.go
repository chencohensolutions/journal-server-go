package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

// Verdict is what Verify found.
type Verdict string

const (
	// VerifyOK: the original is on disk, with its sidecar, and its bytes have the sha256.
	VerifyOK Verdict = "ok"
	// VerifyMissing: the server doesn't have it (not in the index, or the original or sidecar is gone).
	VerifyMissing Verdict = "missing"
	// VerifyMismatch: the original is there but its size or bytes don't match the sha256.
	VerifyMismatch Verdict = "mismatch"
)

// Verify reads the original of sha from disk again and hashes it, so a device can be sure before
// it deletes its own copy. Nothing is remembered between calls. An error means the file could not be
// read (or ctx ended), not that it is wrong.
func (s *Store) Verify(ctx context.Context, sha string) (Verdict, error) {
	// A WAV kept as FLAC is verified by its FLAC.
	item, ok := s.Find(sha)
	if !ok {
		return VerifyMissing, nil
	}
	original := filepath.Join(s.root, filepath.FromSlash(item.Path))
	if _, err := os.Stat(s.sidecarPath(&item)); errors.Is(err, fs.ErrNotExist) {
		log.Printf("media: verify %s: the sidecar is gone", item.Path)
		return VerifyMissing, nil
	}
	file, err := os.Open(original)
	if errors.Is(err, fs.ErrNotExist) {
		log.Printf("media: verify %s: the original is gone", item.Path)
		return VerifyMissing, nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	// A JPEG kept as JPEG XL, asked about by the JPEG's sha256: the JPEG is made again and hashed, so a device deletes
	// its JPEG only when the server can give it back byte for byte.
	if from := item.Sidecar.Facts.EncodedFrom; from != nil && from.Sha256 == sha && item.Sidecar.Facts.IsJXLOfJPEG() {
		if s.JXL == nil {
			return "", ErrNoJXL
		}
		file.Close()
		if _, err := s.JPEGOf(item); errors.Is(err, ErrNotTheJPEG) {
			log.Printf("media: verify %s: THE JPEG XL DOESN'T MAKE ITS JPEG: %v", item.Path, err)
			return VerifyMismatch, nil
		} else if err != nil {
			return "", err
		}
		return VerifyOK, nil
	}
	h := sha256.New()
	n, err := io.Copy(h, contextReader{ctx, file})
	if err != nil {
		return "", err
	}
	if n != item.Sidecar.Facts.Size || hex.EncodeToString(h.Sum(nil)) != item.Sha256() {
		log.Printf("media: verify %s: THE ORIGINAL IS DAMAGED: its bytes don't have its sha256 (%d bytes, the sidecar says %d)",
			item.Path, n, item.Sidecar.Facts.Size)
		return VerifyMismatch, nil
	}
	return VerifyOK, nil
}

// contextReader stops reading once ctx ends, so a dropped request doesn't keep hashing a large video.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// Files hashed at once by VerifyHandler, so checks don't starve uploads and previews of the disk.
const parallelVerifies = 2

// VerifyHandler serves POST /media/{sha256}/verify: the original is read and hashed again, and the
// answer is 200 {"sha256": "…", "result": "ok" | "missing" | "mismatch"}. A device deletes its copy only
// after "ok". It has no time limit, since a large video on a slow disk takes minutes.
func VerifyHandler(lookup StoreLookup) http.Handler {
	slots := make(chan struct{}, parallelVerifies)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		store, ok := storeOf(w, r, lookup)
		if !ok {
			return
		}
		sha := r.PathValue("sha256")
		if !IsSha256(sha) {
			http.Error(w, "not a sha256", http.StatusBadRequest)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-r.Context().Done():
			return
		}
		verdict, err := store.Verify(r.Context(), sha)
		if errors.Is(err, ErrNoJXL) {
			http.Error(w, "a JPEG kept as JPEG XL can't be checked without libjxl", http.StatusServiceUnavailable)
			return
		}
		if err != nil {
			if r.Context().Err() == nil {
				log.Printf("media: verify %s: %v", sha, err)
				http.Error(w, "could not read the file", http.StatusInternalServerError)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"sha256": sha, "result": string(verdict)})
	})
}
