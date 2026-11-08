package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
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
	// Where the file was on the device, as the device names it ("Pictures/Telegram/", an iPhone's album).
	Folder     *string `json:"folder,omitempty"`
	VersionOf  *string `json:"versionOf"`
	PairedWith *string `json:"pairedWith"`
	UploadedBy string  `json:"uploadedBy"`
	// Set by Import, and by devices for a sound recording (Call: an empty one is a call with no one named; EncodedFrom:
	// the WAV a FLAC was made from on the device).
	ImportedFrom *string      `json:"importedFrom,omitempty"`
	Folders      []string     `json:"folders,omitempty"`
	Call         *Call        `json:"call,omitempty"`
	EncodedFrom  *EncodedFrom `json:"encodedFrom,omitempty"`
	// The camera that took it, read from the file before the turn (ReadCamera; the import passes what it read).
	Camera *Camera `json:"camera,omitempty"`
	// Set by an import: the folder kinds counted once for its run (Store.FolderKinds and the run's files), which
	// SourceOf decides its files by. Without it an import counts the store's files for each file.
	FolderKinds *FolderKinds `json:"-"`
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
	case r.Kind != KindPhoto && r.Kind != KindVideo && r.Kind != KindAudio:
		return &InvalidRequestError{"unknown kind " + string(r.Kind)}
	case r.MimeType != "" && !isMimeType(r.MimeType):
		return &InvalidRequestError{"mimeType must look like image/jpeg"}
	case r.Folder != nil && (len(*r.Folder) > 500 || strings.ContainsFunc(*r.Folder, unicode.IsControl)):
		return &InvalidRequestError{"folder must be at most 500 characters, without control characters"}
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

	s.CatchUp()
	// A WAV kept as FLAC is there already.
	if item, ok := s.Find(req.Sha256); ok {
		// A FLAC stored before the phone said which WAV it was made from learns it now.
		if req.EncodedFrom != nil && item.Sha256() == req.Sha256 && item.Sidecar.Facts.EncodedFrom == nil {
			var updated *Item
			if err := s.Turn(func() error { return s.addEncodedFrom(req.Sha256, *req.EncodedFrom, &updated) }); err != nil {
				return StartResult{}, err
			}
			return StartResult{Existing: updated}, nil
		}
		return StartResult{Existing: &item}, nil
	}
	if req.EncodedFrom != nil {
		if item, ok := s.Find(req.EncodedFrom.Sha256); ok {
			return StartResult{Existing: &item}, nil
		}
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
	if item, ok := s.Find(sha); ok {
		return s.uploadedSize(sha, item), true, nil
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
// Repeating a piece after the file is complete (a lost response) reports it complete again. A piece that ends
// early (ErrIncompleteChunk) keeps what arrived; the result's Offset says where the next piece starts.
func (s *Store) WriteChunk(sha string, start, total int64, body io.Reader, length int64) (ChunkResult, error) {
	unlock := s.uploadLocks.Lock(sha)
	defer unlock()

	if item, ok := s.Find(sha); ok {
		return ChunkResult{Offset: s.uploadedSize(sha, item), Item: &item}, nil
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
	if n, err := io.CopyN(partial, body, length); err != nil {
		// The bytes that arrived are kept (they are in order, and the whole file is checked at the end), so a piece
		// that breaks off on mobile data isn't sent again from its start; the result says where to go on from.
		partial.Sync()
		partial.Close()
		return ChunkResult{Offset: start + n}, fmt.Errorf("%w: %v", ErrIncompleteChunk, err)
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

// uploadedSize is the size a device uploading sha sent: the WAV's for a WAV kept as FLAC.
func (s *Store) uploadedSize(sha string, item Item) int64 {
	if e := item.Sidecar.Facts.EncodedFrom; e != nil && e.Sha256 == sha && item.Sidecar.Sha256 != sha {
		return e.Size
	}
	return item.Sidecar.Facts.Size
}

// finish checks the received file, moves it to its month folder and writes its sidecar. A sound recording first gets
// what its file says (soundFacts), a WAV is kept as FLAC (keepAsFLAC), a photo's or video's camera is read
// (cameraFacts), and a JPEG photo is kept as JPEG XL (keepUploadAsJXL), or a device's JPEG XL of a JPEG checked
// (checkDeviceJXL); all run before the turn, as they are slow.
func (s *Store) finish(req UploadRequest) (*Item, error) {
	uploaded := req.Sha256
	partialPath := s.partialPath(uploaded)
	sum, err := HashFile(partialPath)
	if err != nil {
		return nil, err
	}
	if sum != req.Sha256 {
		os.Remove(partialPath)
		os.Remove(s.sessionPath(uploaded))
		return nil, ErrHashMismatch
	}
	from := partialPath
	switch {
	case isDeviceJXLOfJPEG(req):
		if err := s.checkDeviceJXL(partialPath, req); err != nil {
			log.Printf("media: %s from %s refused: %v", req.FileName, req.UploadedBy, err)
			os.Remove(partialPath)
			os.Remove(s.sessionPath(uploaded))
			return nil, err
		}
	case req.Kind == KindPhoto && req.EncodedFrom == nil && IsJPEG(req.FileName, req.MimeType):
		req = s.cameraFacts(partialPath, req) // from the JPEG's own tags, before it is packed
		if jxl, jxlReq, ok := s.keepUploadAsJXL(partialPath, req); ok {
			defer os.Remove(jxl)
			os.Remove(partialPath)
			from, req = jxl, jxlReq
		}
	}
	if req.Kind == KindAudio && IsWAV(req.FileName, req.MimeType) {
		flac, flacReq, err := s.keepAsFLAC(partialPath, req)
		if err != nil {
			return nil, err
		}
		defer os.Remove(flac)
		os.Remove(partialPath)
		from, req = flac, flacReq
	}
	if req.Kind == KindAudio {
		req = s.soundFacts(from, req)
	}
	req = s.cameraFacts(from, req)
	var item *Item
	err = s.Turn(func() error {
		// An import running meanwhile may have placed the same file; or the FLAC of this WAV is there already, and
		// learns the WAV's sha256.
		if existing, ok := s.Get(req.Sha256); ok {
			os.Remove(from)
			if req.EncodedFrom != nil && existing.Sidecar.Facts.EncodedFrom == nil {
				return s.addEncodedFrom(req.Sha256, *req.EncodedFrom, &item)
			}
			item = &existing
			return nil
		}
		var err error
		item, err = s.place(from, req)
		return err
	})
	if err != nil {
		return nil, err
	}
	os.Remove(s.sessionPath(uploaded))
	return item, nil
}

// place moves a file whose sha256 was checked to its month folder and writes its sidecar, during a turn.
func (s *Store) place(from string, req UploadRequest) (*Item, error) {
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
	if err := rename(from, dest); err != nil {
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
			FileName:     req.FileName,
			Kind:         req.Kind,
			MimeType:     mimeType,
			Size:         req.Size,
			TakenAt:      req.TakenAt,
			Width:        req.Width,
			Height:       req.Height,
			DurationMs:   req.DurationMs,
			Location:     req.Location,
			Source:       req.Source,
			SentFrom:     req.Source,
			DeviceFolder: req.Folder,
			VersionOf:    req.VersionOf,
			PairedWith:   req.PairedWith,
			UploadedAt:   s.now().UTC(),
			UploadedBy:   req.UploadedBy,
			ImportedFrom: req.ImportedFrom,
			Folders:      req.Folders,
			Call:         req.Call,
			EncodedFrom:  req.EncodedFrom,
			Camera:       req.Camera,
		},
		Privacy: s.inheritedPrivacy(req),
		Tucked:  s.inheritedTuck(req),
	}
	// The one rule decides the source, after SentFrom keeps what was sent.
	kinds := req.FolderKinds
	switch {
	case kinds != nil:
	case sidecar.Facts.Imported():
		kinds = s.folderKindsLocked()
		kinds.Add(req.Sha256, sidecar.Facts) // the file itself counts, as it does once placed
	default:
		kinds = s.marksOnlyLocked()
	}
	source, app := SourceOf(sidecar.Facts, s.cameraMarks, kinds)
	sidecar.Facts.Source, sidecar.Facts.ScreenshotApp = &source, app
	if err := writeSidecar(dest+".json", sidecar); err != nil {
		// Put the bytes back so the device can finish again; never leave an original without a sidecar.
		rename(dest, from)
		return nil, err
	}
	item := &Item{Path: dirRel + "/" + name, Sidecar: sidecar}
	s.indexItem(item)
	s.wrote(item)
	copied := *item
	return &copied, nil
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

// keepAsFLAC encodes an uploaded WAV as FLAC (ffmpeg) beside it, and returns the FLAC and the request for it: its own
// sha256 and size, named .flac, with EncodedFrom naming the WAV, which isn't kept.
func (s *Store) keepAsFLAC(wav string, req UploadRequest) (string, UploadRequest, error) {
	if s.FFmpeg == "" {
		return "", req, ErrNoEncoder
	}
	work, err := os.MkdirTemp(s.uploadsDir(), req.Sha256+".flac-")
	if err != nil {
		return "", req, err
	}
	// The FLAC is moved out of work before it is used.
	defer os.RemoveAll(work)
	encoded, err := EncodeFLAC(context.Background(), s.FFmpeg, work, wav)
	if err != nil {
		return "", req, err
	}
	flac := filepath.Join(s.uploadsDir(), req.Sha256+".flac")
	if err := rename(encoded, flac); err != nil {
		return "", req, err
	}
	sum, err := HashFile(flac)
	if err != nil {
		os.Remove(flac)
		return "", req, err
	}
	info, err := os.Stat(flac)
	if err != nil {
		os.Remove(flac)
		return "", req, err
	}
	mime := req.MimeType
	if mime == "" || !strings.HasPrefix(strings.ToLower(mime), "audio/") {
		mime = "audio/wav"
	}
	out := req
	out.EncodedFrom = &EncodedFrom{Sha256: req.Sha256, MimeType: mime, Size: req.Size}
	out.Sha256, out.Size, out.MimeType = sum, info.Size(), "audio/flac"
	out.FileName = strings.TrimSuffix(req.FileName, filepath.Ext(req.FileName)) + ".flac"
	return flac, out, nil
}

// soundFacts fills an uploaded sound recording's facts from its file (ProbeAudio): its length and format, when it
// started (the name's local time, or ffprobe's creation time less the length), and who a call was with from its
// name. What ffprobe can't read keeps the device's facts; a device's call keeps its names over the file name's.
func (s *Store) soundFacts(path string, req UploadRequest) UploadRequest {
	a := ProbeAudio(context.Background(), s.FFprobe, path, req.FileName, "", req.TakenAt)
	if a.DurationMs != nil {
		req.DurationMs = a.DurationMs
	}
	if a.MimeType != "" {
		req.MimeType = a.MimeType
	}
	if a.TakenAt != nil {
		req.TakenAt = a.TakenAt
	}
	if a.Call != nil {
		if req.Call == nil {
			req.Call = a.Call
		} else if req.Call.Contact == nil && req.Call.Number == nil {
			req.Call = a.Call
		}
	}
	return req
}

// cameraFacts reads the camera of a photo or video from its file (ReadCamera), when the request has none.
func (s *Store) cameraFacts(path string, req UploadRequest) UploadRequest {
	if req.Camera == nil && req.Kind != KindAudio {
		req.Camera = ReadCamera(context.Background(), s.CameraTools.WithFFprobe(s.FFprobe), path, req.Kind)
	}
	return req
}

// addEncodedFrom records on a stored FLAC the WAV it was made from, during a turn; item is set to it.
func (s *Store) addEncodedFrom(sha string, from EncodedFrom, item **Item) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	found, ok := s.items[sha]
	if !ok {
		return fmt.Errorf("%s isn't in the data folder", sha)
	}
	sidecar := found.Sidecar
	sidecar.Facts.EncodedFrom = &from
	if err := s.rewriteSidecar(found, sidecar); err != nil {
		return err
	}
	copied := *found
	*item = &copied
	return nil
}
