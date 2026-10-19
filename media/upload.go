package media

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// MaxFileSize is the largest original accepted.
	MaxFileSize int64 = 32 << 30
	// ChunkSize is the piece size devices are asked to send.
	ChunkSize int64 = 16 << 20
	// MaxChunkSize is the largest piece accepted in one request.
	MaxChunkSize int64 = 64 << 20
	// UploadLifetime is how long an unfinished upload is kept after it was last started.
	UploadLifetime = 7 * 24 * time.Hour
	// spaceReserve stays free on the disk; an upload that would eat into it is refused.
	spaceReserve uint64 = 1 << 30
)

// UploadRequest is what a device declares before sending a file's bytes.
type UploadRequest struct {
	Sha256     string     `json:"sha256"`
	Size       int64      `json:"size"`
	FileName   string     `json:"fileName"`
	MimeType   string     `json:"mimeType"`
	Kind       Kind       `json:"kind"`
	TakenAt    *time.Time `json:"takenAt"`
	Width      *int       `json:"width"`
	Height     *int       `json:"height"`
	DurationMs *int64     `json:"durationMs"`
	Location   *GeoPoint  `json:"location"`
	Source     *string    `json:"source"`
	VersionOf  *string    `json:"versionOf"`
	PairedWith *string    `json:"pairedWith"`
	UploadedBy string     `json:"uploadedBy"`
}

// InvalidRequestError is a request no device should send (a client bug, not a user error).
type InvalidRequestError struct{ Reason string }

func (e *InvalidRequestError) Error() string { return "invalid upload: " + e.Reason }

func (r UploadRequest) validate() error {
	switch {
	case !IsSha256(r.Sha256):
		return &InvalidRequestError{"sha256 must be 64 lowercase hex characters"}
	case r.Size <= 0:
		return &InvalidRequestError{"size must be positive"}
	case strings.TrimSpace(r.FileName) == "":
		return &InvalidRequestError{"fileName is empty"}
	case r.Kind != KindPhoto && r.Kind != KindVideo:
		return &InvalidRequestError{"unknown kind " + string(r.Kind)}
	}
	for name, ref := range map[string]*string{"versionOf": r.VersionOf, "pairedWith": r.PairedWith} {
		if ref != nil && (!IsSha256(*ref) || *ref == r.Sha256) {
			return &InvalidRequestError{name + " must be the sha256 of another file"}
		}
	}
	return nil
}

// Rejection is why the server won't take a file; the device shows it and doesn't retry.
type Rejection string

const (
	RejectUnknownOriginal Rejection = "UNKNOWN_ORIGINAL"
	RejectUnknownPair     Rejection = "UNKNOWN_PAIR"
	RejectTooLarge        Rejection = "TOO_LARGE"
	RejectNoSpace         Rejection = "NO_SPACE"
)

// StartResult holds exactly one of Existing, Rejected or Upload.
type StartResult struct {
	Existing *Item
	Rejected Rejection
	Upload   *Upload
}

// Upload is where a device continues sending bytes.
type Upload struct {
	Offset    int64
	ExpiresAt time.Time
}

type uploadSession struct {
	Request   UploadRequest `json:"request"`
	ExpiresAt time.Time     `json:"expiresAt"`
}

var (
	ErrNoUpload        = errors.New("no upload was started for this file; call startUpload")
	ErrSizeMismatch    = errors.New("the total size differs from the size given to startUpload")
	ErrBadRange        = errors.New("the piece is outside the file")
	ErrIncompleteChunk = errors.New("the piece ended before its declared length")
	ErrHashMismatch    = errors.New("the bytes received do not have the declared sha256; start over")
)

// WrongOffsetError means the piece doesn't start where the received bytes end.
type WrongOffsetError struct{ Offset int64 }

func (e *WrongOffsetError) Error() string {
	return fmt.Sprintf("expected a piece starting at byte %d", e.Offset)
}

// ChunkResult is the state after a piece: Item is set once the whole file is in place.
type ChunkResult struct {
	Offset int64
	Item   *Item
}

func (s *Store) partialPath(sha string) string { return filepath.Join(s.uploadsDir(), sha+".partial") }
func (s *Store) sessionPath(sha string) string { return filepath.Join(s.uploadsDir(), sha+".json") }

// StartUpload begins an upload, or continues one with the same sha256 and size from where it
// stopped (also after a server restart). A file the store already has is not uploaded again.
func (s *Store) StartUpload(req UploadRequest) (StartResult, error) {
	if err := req.validate(); err != nil {
		return StartResult{}, err
	}
	unlock := s.uploadLocks.Lock(req.Sha256)
	defer unlock()

	if item, ok := s.Get(req.Sha256); ok {
		return StartResult{Existing: &item}, nil
	}
	if req.VersionOf != nil {
		if _, ok := s.Get(*req.VersionOf); !ok {
			return StartResult{Rejected: RejectUnknownOriginal}, nil
		}
	}
	if req.PairedWith != nil {
		if _, ok := s.Get(*req.PairedWith); !ok {
			return StartResult{Rejected: RejectUnknownPair}, nil
		}
	}
	if req.Size > MaxFileSize {
		return StartResult{Rejected: RejectTooLarge}, nil
	}

	var offset int64
	if previous, err := s.loadSession(req.Sha256); err == nil && previous.Request.Size == req.Size {
		if info, err := os.Stat(s.partialPath(req.Sha256)); err == nil {
			offset = info.Size()
		}
	} else if err := os.Remove(s.partialPath(req.Sha256)); err != nil && !os.IsNotExist(err) {
		return StartResult{}, err
	}
	if free, err := freeBytes(s.root); err == nil && free < uint64(req.Size-offset)+spaceReserve {
		return StartResult{Rejected: RejectNoSpace}, nil
	}

	partial, err := os.OpenFile(s.partialPath(req.Sha256), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return StartResult{}, err
	}
	if err := partial.Close(); err != nil {
		return StartResult{}, err
	}
	session := uploadSession{Request: req, ExpiresAt: s.now().Add(UploadLifetime).UTC()}
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return StartResult{}, err
	}
	if err := writeFileAtomic(s.sessionPath(req.Sha256), data); err != nil {
		return StartResult{}, err
	}
	return StartResult{Upload: &Upload{Offset: offset, ExpiresAt: session.ExpiresAt}}, nil
}

func (s *Store) loadSession(sha string) (uploadSession, error) {
	var session uploadSession
	data, err := os.ReadFile(s.sessionPath(sha))
	if err != nil {
		return session, err
	}
	err = json.Unmarshal(data, &session)
	return session, err
}

// Offset is how many bytes of the file have arrived; complete when the file is already stored.
func (s *Store) Offset(sha string) (offset int64, complete bool, err error) {
	unlock := s.uploadLocks.Lock(sha)
	defer unlock()
	if item, ok := s.Get(sha); ok {
		return item.Sidecar.Facts.Size, true, nil
	}
	if _, err := s.loadSession(sha); err != nil {
		return 0, false, ErrNoUpload
	}
	info, err := os.Stat(s.partialPath(sha))
	if err != nil {
		return 0, false, ErrNoUpload
	}
	return info.Size(), false, nil
}

// WriteChunk appends length bytes from body at start, which must be where the received bytes end.
// After the last piece the whole file is checked against its sha256 and moved into media/.
// Repeating a piece after the file is complete (a lost response) reports it complete again.
func (s *Store) WriteChunk(sha string, start, total int64, body io.Reader, length int64) (ChunkResult, error) {
	unlock := s.uploadLocks.Lock(sha)
	defer unlock()

	if item, ok := s.Get(sha); ok {
		return ChunkResult{Offset: item.Sidecar.Facts.Size, Item: &item}, nil
	}
	session, err := s.loadSession(sha)
	if err != nil {
		return ChunkResult{}, ErrNoUpload
	}
	if total != session.Request.Size {
		return ChunkResult{}, ErrSizeMismatch
	}
	if start < 0 || length <= 0 || start+length > total {
		return ChunkResult{}, ErrBadRange
	}

	partial, err := os.OpenFile(s.partialPath(sha), os.O_WRONLY, 0)
	if err != nil {
		return ChunkResult{}, ErrNoUpload
	}
	info, err := partial.Stat()
	if err != nil {
		partial.Close()
		return ChunkResult{}, err
	}
	if info.Size() != start {
		partial.Close()
		return ChunkResult{}, &WrongOffsetError{Offset: info.Size()}
	}
	if _, err := partial.Seek(start, io.SeekStart); err != nil {
		partial.Close()
		return ChunkResult{}, err
	}
	if _, err := io.CopyN(partial, body, length); err != nil {
		// Keep only whole pieces, so the next try starts at a known offset.
		partial.Truncate(start)
		partial.Close()
		return ChunkResult{}, fmt.Errorf("%w: %v", ErrIncompleteChunk, err)
	}
	if err := partial.Sync(); err != nil {
		partial.Close()
		return ChunkResult{}, err
	}
	if err := partial.Close(); err != nil {
		return ChunkResult{}, err
	}

	if offset := start + length; offset < total {
		return ChunkResult{Offset: offset}, nil
	}
	item, err := s.finish(session.Request)
	if err != nil {
		return ChunkResult{}, err
	}
	return ChunkResult{Offset: total, Item: item}, nil
}

// finish checks the received file, moves it to its month folder and writes its sidecar.
func (s *Store) finish(req UploadRequest) (*Item, error) {
	partialPath := s.partialPath(req.Sha256)
	sum, err := hashFile(partialPath)
	if err != nil {
		return nil, err
	}
	if sum != req.Sha256 {
		os.Remove(partialPath)
		os.Remove(s.sessionPath(req.Sha256))
		return nil, ErrHashMismatch
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	dirRel := monthDir(req.TakenAt)
	dir := filepath.Join(s.root, filepath.FromSlash(dirRel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	name, err := freeName(dir, safeFileName(req.FileName))
	if err != nil {
		return nil, err
	}
	dest := filepath.Join(dir, name)
	if err := rename(partialPath, dest); err != nil {
		return nil, err
	}

	mimeType := req.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	sidecar := Sidecar{
		Format: SidecarFormat,
		Sha256: req.Sha256,
		Facts: Facts{
			FileName:   req.FileName,
			Kind:       req.Kind,
			MimeType:   mimeType,
			Size:       req.Size,
			TakenAt:    req.TakenAt,
			Width:      req.Width,
			Height:     req.Height,
			DurationMs: req.DurationMs,
			Location:   req.Location,
			Source:     req.Source,
			VersionOf:  req.VersionOf,
			PairedWith: req.PairedWith,
			UploadedAt: s.now().UTC(),
			UploadedBy: req.UploadedBy,
		},
	}
	data, err := json.MarshalIndent(sidecar, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(dest+".json", data); err != nil {
		// Put the bytes back so the device can finish again; never leave an original without a sidecar.
		rename(dest, partialPath)
		return nil, err
	}
	item := &Item{Path: dirRel + "/" + name, Sidecar: sidecar}
	s.items[req.Sha256] = item
	os.Remove(s.sessionPath(req.Sha256))
	copied := *item
	return &copied, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// PurgeExpiredUploads deletes unfinished uploads past their expiry, and pieces without a session.
func (s *Store) PurgeExpiredUploads() error {
	entries, err := os.ReadDir(s.uploadsDir())
	if err != nil {
		return err
	}
	now := s.now()
	for _, entry := range entries {
		name := entry.Name()
		sha, ext, _ := strings.Cut(name, ".")
		if !IsSha256(sha) || (ext != "json" && ext != "partial") {
			continue
		}
		func() {
			unlock := s.uploadLocks.Lock(sha)
			defer unlock()
			session, err := s.loadSession(sha)
			if err == nil && now.Before(session.ExpiresAt) {
				return
			}
			if ext == "partial" && err == nil {
				return // handled with its session
			}
			os.Remove(s.partialPath(sha))
			os.Remove(s.sessionPath(sha))
		}()
	}
	return nil
}
