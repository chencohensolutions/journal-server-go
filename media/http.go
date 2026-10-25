package media

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"strconv"
	"time"
)

// StoreLookup finds the store of the request's user; nil when the user has no data folder.
type StoreLookup func(r *http.Request) *Store

// storeOf answers the request with 404 when its user has no data folder.
func storeOf(w http.ResponseWriter, r *http.Request, lookup StoreLookup) (*Store, bool) {
	store := lookup(r)
	if store == nil {
		http.Error(w, "no data folder is configured for this user", http.StatusNotFound)
	}
	return store, store != nil
}

// Bytes of one piece may arrive slowly on mobile data, but a stalled connection must not hold the
// upload lock forever.
const chunkReadTimeout = 10 * time.Minute

// UploadHandler serves PUT /upload/{sha256} (one piece, "Content-Range: bytes start-end/total")
// and HEAD /upload/{sha256} (how many bytes arrived, in Upload-Offset).
//
//	200 Upload-Offset: n       more pieces expected
//	201 Upload-Offset: total   the file is stored (also when a piece is repeated afterwards)
//	400 Upload-Offset: n       the piece ended early; what arrived is kept, go on from n (no header: a bad request)
//	404                        no upload started (call startUpload), or it expired
//	409                        the bytes don't match the sha256; the upload is gone, start over
//	416 Upload-Offset: n       the piece doesn't start at n; continue from n
func UploadHandler(lookup StoreLookup) http.Handler {
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

		if r.Method == http.MethodHead {
			offset, complete, err := store.Offset(sha)
			if err != nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Upload-Offset", strconv.FormatInt(offset, 10))
			w.Header().Set("Upload-Complete", strconv.FormatBool(complete))
			w.WriteHeader(http.StatusOK)
			return
		}

		start, end, total, err := parseContentRange(r.Header.Get("Content-Range"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		length := end - start + 1
		if length > MaxChunkSize {
			http.Error(w, fmt.Sprintf("pieces may be at most %d bytes", MaxChunkSize), http.StatusRequestEntityTooLarge)
			return
		}
		if r.ContentLength >= 0 && r.ContentLength != length {
			http.Error(w, "Content-Length must match Content-Range", http.StatusBadRequest)
			return
		}
		if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(chunkReadTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			log.Printf("media: upload read deadline: %v", err)
		}

		result, err := store.WriteChunk(sha, start, total, http.MaxBytesReader(w, r.Body, length), length)
		var wrongOffset *WrongOffsetError
		var invalid *InvalidRequestError
		switch {
		case err == nil:
		case errors.As(err, &wrongOffset):
			w.Header().Set("Upload-Offset", strconv.FormatInt(wrongOffset.Offset, 10))
			http.Error(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
			return
		case errors.Is(err, ErrNoUpload):
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		case errors.Is(err, ErrHashMismatch):
			http.Error(w, err.Error(), http.StatusConflict)
			return
		case errors.Is(err, ErrIncompleteChunk):
			w.Header().Set("Upload-Offset", strconv.FormatInt(result.Offset, 10))
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		case errors.Is(err, ErrSizeMismatch), errors.Is(err, ErrBadRange), errors.As(err, &invalid):
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		default:
			log.Printf("media: upload %s: %v", sha, err)
			http.Error(w, "could not store the piece", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Upload-Offset", strconv.FormatInt(result.Offset, 10))
		w.Header().Set("Content-Type", "application/json")
		status := http.StatusOK
		if result.Item != nil {
			status = http.StatusCreated
			log.Printf("media: stored %s (%d bytes)", result.Item.Path, result.Offset)
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"sha256": sha, "offset": result.Offset, "complete": result.Item != nil})
	})
}

// parseContentRange reads "bytes start-end/total".
func parseContentRange(value string) (start, end, total int64, err error) {
	if _, err := fmt.Sscanf(value, "bytes %d-%d/%d", &start, &end, &total); err != nil {
		return 0, 0, 0, errors.New(`Content-Range must be "bytes start-end/total"`)
	}
	if start < 0 || end < start || total <= end {
		return 0, 0, 0, errors.New("Content-Range is outside the file")
	}
	return start, end, total, nil
}

// OriginalHandler serves GET /media/{sha256}: the original, byte for byte, with Range requests,
// so videos stream and seek. The content never changes, so clients may cache it for good.
func OriginalHandler(lookup StoreLookup) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		store, ok := storeOf(w, r, lookup)
		if !ok {
			return
		}
		sha := r.PathValue("sha256")
		item, ok := store.Get(sha)
		if !ok {
			http.NotFound(w, r)
			return
		}
		file, err := store.Open(item)
		if err != nil {
			log.Printf("media: open %s: %v", item.Path, err)
			http.Error(w, "could not read the file", http.StatusInternalServerError)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", item.Sidecar.Facts.MimeType)
		w.Header().Set("ETag", `"`+sha+`"`)
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		http.ServeContent(w, r, path.Base(item.Path), item.Sidecar.Facts.UploadedAt, file)
	})
}

// PreviewHandler serves GET /media/{sha256}/preview?w=&h=&fit=&crop=&fmt=&q=: a photo scaled down once
// from the original to exactly the pixels the client will draw (see PreviewSpec); for a video, its
// poster frame (see poster.go). A sound recording has none: 404.
func PreviewHandler(lookup StoreLookup, previews *Previews) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		store, ok := storeOf(w, r, lookup)
		if !ok {
			return
		}
		sha := r.PathValue("sha256")
		item, ok := store.Get(sha)
		if !ok {
			http.NotFound(w, r)
			return
		}
		spec, err := ParsePreviewSpec(r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		path, release, err := previews.Get(r.Context(), store, item, spec)
		defer release()
		switch {
		case errors.Is(err, ErrNoPicture):
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		case errors.Is(err, ErrPreviewsUnavailable), errors.Is(err, ErrPostersUnavailable):
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		case err != nil:
			log.Printf("media: preview of %s: %v", item.Path, err)
			http.Error(w, "could not make the preview", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", spec.ContentType())
		w.Header().Set("ETag", `"`+sha+"-"+spec.fileName()+`"`)
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		http.ServeFile(w, r, path)
	})
}
