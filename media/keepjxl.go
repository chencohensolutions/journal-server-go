package media

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Keeping JPEGs as JPEG XL (the owner's choice, Oct 28, 2026): each JPEG photo is packed again as JPEG XL without
// losing a bit (JXL.Keep checks that the very same JPEG comes back), about 20% smaller. The JPEG XL is the file
// from then on, under its own sha256; its sidecar keeps the JPEG's sha256, size, type and name in encodedFrom, as a
// WAV kept as FLAC does, so a device or an import holding the JPEG finds it (Store.Find), and the JPEG can be made
// again (JPEGOf). New uploads are kept so as they arrive (finish), and journal-server jxl does the files already there.

var (
	// ErrNotJPEG: only JPEG photos are kept as JPEG XL.
	ErrNotJPEG = errors.New("not a JPEG photo")
	// ErrNoSaving: the JPEG XL wouldn't be smaller, so the JPEG stays.
	ErrNoSaving = errors.New("the JPEG XL isn't smaller than the JPEG")
	// ErrNotTheJPEG: a JPEG XL a device sent doesn't make the JPEG it says it was made from.
	ErrNotTheJPEG = errors.New("the JPEG XL doesn't make the JPEG it names in encodedFrom")
)

// KeepAsJXL replaces the JPEG photo sha by its JPEG XL, in the same month folder (named as the JPEG, .jxl), and
// returns the JPEG XL's item. Its sidecar is the JPEG's (facts, privacy, scans, the note) with the JPEG XL's sha256,
// size and type, the JPEG in encodedFrom, and correctedAt now, so devices get it in the change feed; edits and paired
// files that named the JPEG name the JPEG XL. The packing and checking happen before the turn; in the turn the JPEG XL
// and its sidecar are placed first and the JPEG deleted after, all recorded first (replacing), so a turn stopped between
// is finished or undone when the data folder is next opened. A JPEG libjxl can't keep, or that wouldn't get smaller,
// stays (an error).
func (s *Store) KeepAsJXL(sha string) (Item, error) {
	if s.JXL == nil {
		return Item{}, ErrNoJXL
	}
	item, ok := s.Get(sha)
	if !ok {
		return Item{}, fmt.Errorf("%s isn't in the data folder", sha)
	}
	facts := item.Sidecar.Facts
	if facts.Kind != KindPhoto || facts.EncodedFrom != nil || !IsJPEG(facts.FileName, facts.MimeType) {
		return Item{}, ErrNotJPEG
	}
	jpeg, err := os.ReadFile(filepath.Join(s.root, filepath.FromSlash(item.Path)))
	if err != nil {
		return Item{}, err
	}
	if sum := sha256.Sum256(jpeg); hex.EncodeToString(sum[:]) != sha {
		return Item{}, fmt.Errorf("%s: THE ORIGINAL IS DAMAGED: its bytes don't have its sha256; left as it is", item.Path)
	}
	jxl, err := s.JXL.Keep(jpeg)
	if err != nil {
		return Item{}, err
	}
	if len(jxl) >= len(jpeg) {
		return Item{}, ErrNoSaving
	}
	tmp, err := os.CreateTemp(s.uploadsDir(), sha+".*.jxl")
	if err != nil {
		return Item{}, err
	}
	defer os.Remove(tmp.Name()) // moved away once placed
	if _, err := tmp.Write(jxl); err != nil {
		tmp.Close()
		return Item{}, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return Item{}, err
	}
	if err := tmp.Close(); err != nil {
		return Item{}, err
	}
	sum := sha256.Sum256(jxl)
	var kept Item
	err = s.Turn(func() error {
		var err error
		kept, err = s.replaceByJXL(item, tmp.Name(), hex.EncodeToString(sum[:]), int64(len(jxl)))
		return err
	})
	return kept, err
}

// replaceByJXL places the JPEG XL at from beside the JPEG item and deletes the JPEG, during a turn.
func (s *Store) replaceByJXL(jpeg Item, from, sha string, size int64) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	found, ok := s.items[jpeg.Sha256()]
	if !ok || found.Path != jpeg.Path || found.Sidecar.Facts.EncodedFrom != nil {
		return Item{}, fmt.Errorf("%s changed while its JPEG XL was made; left as it is", jpeg.Path)
	}
	if other, ok := s.items[sha]; ok {
		return Item{}, fmt.Errorf("%s's JPEG XL is in the data folder already, as %s", jpeg.Path, other.Path)
	}
	dirRel := path.Dir(found.Path)
	dir := filepath.Join(s.root, filepath.FromSlash(dirRel))
	name, err := freeName(dir, strings.TrimSuffix(path.Base(found.Path), path.Ext(found.Path))+".jxl")
	if err != nil {
		return Item{}, err
	}
	dest := filepath.Join(dir, name)
	// Recorded before anything moves, and deleted once all of it is done or undone: a turn stopped between (the process
	// killed, the power cut) is finished or undone when the data folder is next opened (finishReplacements).
	record, err := s.recordReplacing(replacing{JPEG: found.Path, JPEGSha: found.Sidecar.Sha256, JXL: dirRel + "/" + name, JXLSha: sha})
	if err != nil {
		return Item{}, err
	}
	defer os.Remove(record)
	if err := rename(from, dest); err != nil {
		return Item{}, err
	}
	now := s.now().UTC()
	old := found.Sidecar
	sidecar := old
	sidecar.Sha256 = sha
	sidecar.Facts.EncodedFrom = &EncodedFrom{Sha256: old.Sha256, MimeType: old.Facts.MimeType, Size: old.Facts.Size,
		FileName: old.Facts.FileName}
	sidecar.Facts.FileName = strings.TrimSuffix(old.Facts.FileName, filepath.Ext(old.Facts.FileName)) + ".jxl"
	sidecar.Facts.MimeType = "image/jxl"
	sidecar.Facts.Size = size
	sidecar.Facts.CorrectedAt = &now
	if err := writeSidecar(dest+".json", sidecar); err != nil {
		if rename(dest, from) != nil {
			os.Remove(dest)
		}
		return Item{}, err
	}
	// The JPEG goes once its JPEG XL is in place: its sidecar first, so what a stop leaves is a file no sidecar names.
	jpegPath := filepath.Join(s.root, filepath.FromSlash(found.Path))
	if err := os.Remove(jpegPath + ".json"); err != nil {
		log.Printf("media: %s is kept as %s, but its sidecar could not be deleted: %v", found.Path, name, err)
	} else if err := os.Remove(jpegPath); err != nil {
		log.Printf("media: %s is kept as %s, but it could not be deleted: %v", found.Path, name, err)
	}
	item := &Item{Path: dirRel + "/" + name, Sidecar: sidecar}
	s.indexItem(item) // and the JPEG leaves the index
	s.wrote(item)
	s.relinkToJXL(old.Sha256, item, now)
	return *item, nil
}

// relinkToJXL has the edits and paired files (a RAW's JPEG, a Live Photo's still) that named the JPEG jpegSha name its
// JPEG XL jxl. Callers hold s.mu, in a turn.
func (s *Store) relinkToJXL(jpegSha string, jxl *Item, now time.Time) {
	sha := jxl.Sidecar.Sha256
	for _, other := range s.items {
		f := other.Sidecar.Facts
		if (f.VersionOf == nil || *f.VersionOf != jpegSha) && (f.PairedWith == nil || *f.PairedWith != jpegSha) {
			continue
		}
		changed := other.Sidecar
		if f.VersionOf != nil && *f.VersionOf == jpegSha {
			changed.Facts.VersionOf = &sha
		}
		if f.PairedWith != nil && *f.PairedWith == jpegSha {
			changed.Facts.PairedWith = &sha
		}
		changed.Facts.CorrectedAt = &now
		if err := s.rewriteSidecar(other, changed); err != nil {
			log.Printf("media: %s named a JPEG kept as %s; it still names the JPEG: %v", other.Path, jxl.Path, err)
		}
	}
}

// replacing is what a turn records before it replaces a JPEG by its JPEG XL (in .cache/jxl/replacing/, one file per
// JPEG) and deletes once all of it is done or undone. One still there when the data folder is opened is a turn that was
// stopped halfway: finishReplacements finishes or undoes it, so a stop never leaves a spare copy behind.
type replacing struct {
	JPEG    string `json:"jpeg"` // the JPEG's path in the data folder
	JPEGSha string `json:"jpegSha256"`
	JXL     string `json:"jxl"` // where its JPEG XL goes
	JXLSha  string `json:"jxlSha256"`
}

func (s *Store) replacingDir() string { return filepath.Join(s.root, ".cache", "jxl", "replacing") }

// recordReplacing writes r, whole or not at all, and returns its path.
func (s *Store) recordReplacing(r replacing) (string, error) {
	if err := os.MkdirAll(s.replacingDir(), 0o755); err != nil {
		return "", err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	path := filepath.Join(s.replacingDir(), r.JPEGSha+".json")
	return path, writeFileAtomic(path, data)
}

// finishReplacements finishes or undoes each JPEG-to-JPEG XL replacement a stopped turn left recorded, in a turn:
//   - its JPEG XL's sidecar was written: the replacement is finished. The JPEG XL is checked whole (its sha256), then the
//     JPEG's sidecar and the JPEG (checked to be that JPEG) are deleted, and edits that named the JPEG name the JPEG XL.
//   - it wasn't: the replacement is undone. The JPEG XL placed beside the JPEG (checked to be that JPEG XL) is deleted;
//     the JPEG and its sidecar were never touched.
//
// Either way the half-written temp files of a stopped write go, and so do JPEG XLs a stopped KeepAsJXL left in
// .cache/uploads. A record it can't settle stays, for the next time, and is logged.
func (s *Store) finishReplacements() error {
	s.removeStaleKeepTemps()
	entries, err := os.ReadDir(s.replacingDir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range entries {
		path := filepath.Join(s.replacingDir(), entry.Name())
		var r replacing
		data, err := os.ReadFile(path)
		if err != nil || !strings.HasSuffix(entry.Name(), ".json") || json.Unmarshal(data, &r) != nil || r.JPEG == "" || r.JXL == "" {
			os.Remove(path) // a record's own temp file, stopped before it was in place: nothing was moved
			continue
		}
		if err := s.finishReplacing(r); err != nil {
			log.Printf("media: %s was being kept as JPEG XL when a run stopped, and is left as it is: %v", r.JPEG, err)
			continue
		}
		os.Remove(path)
	}
	return nil
}

func (s *Store) finishReplacing(r replacing) error {
	jpegPath := filepath.Join(s.root, filepath.FromSlash(r.JPEG))
	jxlPath := filepath.Join(s.root, filepath.FromSlash(r.JXL))
	defer removeWriteTemps(jxlPath + ".json")
	defer removeWriteTemps(jpegPath + ".json")
	data, err := os.ReadFile(jxlPath + ".json")
	if os.IsNotExist(err) {
		switch sum, err := HashFile(jxlPath); {
		case os.IsNotExist(err):
		case err != nil:
			return err
		case sum != r.JXLSha:
			return fmt.Errorf("%s isn't the JPEG XL it was placing", r.JXL)
		default:
			if err := os.Remove(jxlPath); err != nil {
				return err
			}
		}
		log.Printf("media: %s was being kept as JPEG XL when a run stopped; undone, it stays a JPEG", r.JPEG)
		return nil
	}
	if err != nil {
		return err
	}
	var sidecar Sidecar
	if err := json.Unmarshal(data, &sidecar); err != nil {
		return fmt.Errorf("%s.json: %w", r.JXL, err)
	}
	if e := sidecar.Facts.EncodedFrom; sidecar.Sha256 != r.JXLSha || e == nil || e.Sha256 != r.JPEGSha {
		return fmt.Errorf("%s.json isn't the sidecar it was writing", r.JXL)
	}
	if sum, err := HashFile(jxlPath); err != nil || sum != r.JXLSha {
		return fmt.Errorf("its JPEG XL %s isn't whole (%v)", r.JXL, err)
	}
	if err := os.Remove(jpegPath + ".json"); err != nil && !os.IsNotExist(err) {
		return err
	}
	switch sum, err := HashFile(jpegPath); {
	case os.IsNotExist(err):
	case err != nil:
		return err
	case sum != r.JPEGSha:
		log.Printf("media: %s isn't the JPEG kept as %s; left there", r.JPEG, r.JXL)
	default:
		if err := os.Remove(jpegPath); err != nil {
			return err
		}
	}
	item := &Item{Path: r.JXL, Sidecar: sidecar}
	s.indexItem(item)
	s.wrote(item)
	s.relinkToJXL(r.JPEGSha, item, s.now().UTC())
	log.Printf("media: %s was being kept as JPEG XL when a run stopped; finished, as %s", r.JPEG, r.JXL)
	return nil
}

// removeWriteTemps deletes the temp files a stopped writeFileAtomic of path left beside it.
func removeWriteTemps(path string) {
	temps, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp"))
	for _, temp := range temps {
		os.Remove(temp)
	}
}

// removeStaleKeepTemps deletes the JPEG XLs a stopped KeepAsJXL left in .cache/uploads ("<sha256>.<n>.jxl", made
// before its turn) once they are an hour old, so as not to take one another process is packing right now.
func (s *Store) removeStaleKeepTemps() {
	temps, _ := filepath.Glob(filepath.Join(s.uploadsDir(), "*.*.jxl"))
	for _, temp := range temps {
		if info, err := os.Stat(temp); err == nil && time.Since(info.ModTime()) > time.Hour {
			os.Remove(temp)
		}
	}
}

// JPEGOf makes again the JPEG a JPEG XL was kept from (item.Sidecar.Facts.IsJXLOfJPEG), checked against the JPEG's
// sha256 and size.
func (s *Store) JPEGOf(item Item) ([]byte, error) {
	if !item.Sidecar.Facts.IsJXLOfJPEG() {
		return nil, ErrNotJPEG
	}
	if s.JXL == nil {
		return nil, ErrNoJXL
	}
	file, err := s.Open(item)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(file)
	file.Close()
	if err != nil {
		return nil, err
	}
	jpeg, err := s.JXL.ToJPEG(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %v", item.Path, ErrNotTheJPEG, err)
	}
	if err := checkJPEG(jpeg, *item.Sidecar.Facts.EncodedFrom); err != nil {
		return nil, fmt.Errorf("%s: %w", item.Path, err)
	}
	return jpeg, nil
}

// checkJPEG reports ErrNotTheJPEG unless jpeg is the file from names.
func checkJPEG(jpeg []byte, from EncodedFrom) error {
	sum := sha256.Sum256(jpeg)
	if int64(len(jpeg)) != from.Size || hex.EncodeToString(sum[:]) != from.Sha256 {
		return ErrNotTheJPEG
	}
	return nil
}

// keepUploadAsJXL packs an uploaded JPEG photo as JPEG XL beside it (JXL.Keep), and returns the JPEG XL and the request
// for it: its own sha256 and size, named .jxl, with EncodedFrom naming the JPEG, which isn't kept. ok is false when the
// JPEG stays as it is: no libjxl, a JPEG it can't keep, or no saving.
func (s *Store) keepUploadAsJXL(partial string, req UploadRequest) (jxlPath string, out UploadRequest, ok bool) {
	if s.JXL == nil {
		return "", req, false
	}
	jpeg, err := os.ReadFile(partial)
	if err != nil {
		log.Printf("media: %s stays a JPEG: %v", req.FileName, err)
		return "", req, false
	}
	jxl, err := s.JXL.Keep(jpeg)
	if err == nil && len(jxl) >= len(jpeg) {
		err = ErrNoSaving
	}
	if err != nil {
		log.Printf("media: %s stays a JPEG: %v", req.FileName, err)
		return "", req, false
	}
	jxlPath = filepath.Join(s.uploadsDir(), req.Sha256+".jxl")
	if err := writeFileAtomic(jxlPath, jxl); err != nil {
		log.Printf("media: %s stays a JPEG: %v", req.FileName, err)
		return "", req, false
	}
	sum := sha256.Sum256(jxl)
	mime := req.MimeType
	if !IsJPEG("", mime) {
		mime = "image/jpeg"
	}
	out = req
	out.EncodedFrom = &EncodedFrom{Sha256: req.Sha256, MimeType: mime, Size: req.Size, FileName: req.FileName}
	out.Sha256, out.Size, out.MimeType = hex.EncodeToString(sum[:]), int64(len(jxl)), "image/jxl"
	out.FileName = strings.TrimSuffix(req.FileName, filepath.Ext(req.FileName)) + ".jxl"
	return jxlPath, out, true
}

// checkDeviceJXL checks that a JPEG XL a device made from a JPEG (req.EncodedFrom) makes that JPEG again, byte for
// byte, before it is placed: the device may delete its JPEG once the server has it. Without libjxl it can't be
// checked, and is taken (Verify can't vouch for the JPEG until libjxl is there).
func (s *Store) checkDeviceJXL(path string, req UploadRequest) error {
	if s.JXL == nil {
		log.Printf("media: %s: a JPEG XL of a JPEG, taken unchecked: %v", req.FileName, ErrNoJXL)
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	jpeg, err := s.JXL.ToJPEG(data)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotTheJPEG, err)
	}
	return checkJPEG(jpeg, *req.EncodedFrom)
}

// isDeviceJXLOfJPEG reports whether an upload is a JPEG XL the device made from a JPEG.
func isDeviceJXLOfJPEG(req UploadRequest) bool {
	return req.EncodedFrom != nil && req.Kind == KindPhoto && IsJPEG(req.EncodedFrom.FileName, req.EncodedFrom.MimeType) &&
		(strings.EqualFold(req.MimeType, "image/jxl") || strings.EqualFold(filepath.Ext(req.FileName), ".jxl"))
}
