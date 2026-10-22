package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	t     *testing.T
	root  string
	now   time.Time
	store *Store
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{t: t, root: t.TempDir(), now: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)}
	f.reopen()
	return f
}

func (f *fixture) reopen() {
	f.t.Helper()
	store, err := OpenStore(f.root, func() time.Time { return f.now })
	if err != nil {
		f.t.Fatal(err)
	}
	f.store = store
}

func shaOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

var takenInTelAviv = time.Date(2026, 10, 17, 15, 52, 22, 0, time.FixedZone("", 3*3600))

func photoRequest(data []byte, name string) UploadRequest {
	return UploadRequest{
		Sha256: shaOf(data), Size: int64(len(data)), FileName: name, MimeType: "image/heic",
		Kind: KindPhoto, TakenAt: &takenInTelAviv, UploadedBy: "Galaxy S25",
	}
}

func (f *fixture) start(req UploadRequest) StartResult {
	f.t.Helper()
	result, err := f.store.StartUpload(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return result
}

func (f *fixture) chunk(data []byte, start, end int64) (ChunkResult, error) {
	return f.store.WriteChunk(shaOf(data), start, int64(len(data)), bytes.NewReader(data[start:end]), end-start)
}

// upload sends data in one piece and returns the stored item.
func (f *fixture) upload(data []byte, name string) Item {
	f.t.Helper()
	f.start(photoRequest(data, name))
	result, err := f.chunk(data, 0, int64(len(data)))
	if err != nil || result.Item == nil {
		f.t.Fatalf("upload %s: %+v %v", name, result, err)
	}
	return *result.Item
}

func TestUploadInPiecesStoresOriginalAndSidecar(t *testing.T) {
	f := newFixture(t)
	data := []byte("pretend this is a HEIC photo of the beach")
	sha := shaOf(data)

	ready := f.start(photoRequest(data, "IMG_1234.HEIC"))
	if ready.Upload == nil || ready.Upload.Offset != 0 {
		t.Fatalf("start = %+v", ready)
	}
	if missing := f.store.Missing([]string{sha, sha}); len(missing) != 1 {
		t.Fatalf("missing during upload = %v", missing)
	}
	for _, piece := range [][2]int64{{0, 10}, {10, 30}} {
		if result, err := f.chunk(data, piece[0], piece[1]); err != nil || result.Item != nil || result.Offset != piece[1] {
			t.Fatalf("piece %v = %+v %v", piece, result, err)
		}
	}
	result, err := f.chunk(data, 30, int64(len(data)))
	if err != nil || result.Item == nil {
		t.Fatalf("last piece = %+v %v", result, err)
	}
	if result.Item.Path != "media/2026/2026-10/IMG_1234.HEIC" {
		t.Fatalf("path = %s", result.Item.Path)
	}

	stored, _ := os.ReadFile(filepath.Join(f.root, "media/2026/2026-10/IMG_1234.HEIC"))
	if !bytes.Equal(stored, data) {
		t.Fatalf("stored bytes differ")
	}
	var sidecar Sidecar
	raw, _ := os.ReadFile(filepath.Join(f.root, "media/2026/2026-10/IMG_1234.HEIC.json"))
	if err := json.Unmarshal(raw, &sidecar); err != nil {
		t.Fatal(err)
	}
	if sidecar.Format != 1 || sidecar.Sha256 != sha || sidecar.Facts.UploadedBy != "Galaxy S25" || !sidecar.Facts.TakenAt.Equal(takenInTelAviv) {
		t.Fatalf("sidecar = %s", raw)
	}
	if leftovers, _ := os.ReadDir(f.store.uploadsDir()); len(leftovers) != 0 {
		t.Fatalf("uploads folder not cleaned: %v", leftovers)
	}
	if len(f.store.Missing([]string{sha})) != 0 {
		t.Fatalf("still missing after upload")
	}

	// The index is rebuilt from the files.
	f.reopen()
	if item, ok := f.store.Get(sha); !ok || item.Path != result.Item.Path {
		t.Fatalf("after reopen = %+v %v", item, ok)
	}
	// Starting again reports the file as there; repeating the last piece reports it complete.
	if again := f.start(photoRequest(data, "IMG_1234.HEIC")); again.Existing == nil {
		t.Fatalf("second start = %+v", again)
	}
	if repeat, err := f.chunk(data, 30, int64(len(data))); err != nil || repeat.Item == nil {
		t.Fatalf("repeated last piece = %+v %v", repeat, err)
	}
}

func TestUploadResumesAfterRestartAndRejectsWrongOffset(t *testing.T) {
	f := newFixture(t)
	data := bytes.Repeat([]byte("video"), 100)
	f.start(photoRequest(data, "VID.mp4"))
	if _, err := f.chunk(data, 0, 200); err != nil {
		t.Fatal(err)
	}

	f.reopen()
	resumed := f.start(photoRequest(data, "VID.mp4"))
	if resumed.Upload == nil || resumed.Upload.Offset != 200 {
		t.Fatalf("resume = %+v", resumed)
	}
	var wrong *WrongOffsetError
	if _, err := f.chunk(data, 100, 300); !errors.As(err, &wrong) || wrong.Offset != 200 {
		t.Fatalf("overlapping piece: %v", err)
	}
	if offset, complete, err := f.store.Offset(shaOf(data)); err != nil || offset != 200 || complete {
		t.Fatalf("offset = %d %v %v", offset, complete, err)
	}
	// A piece that ends early keeps what arrived.
	short := bytes.NewReader(data[200:250])
	if result, err := f.store.WriteChunk(shaOf(data), 200, int64(len(data)), short, 100); !errors.Is(err, ErrIncompleteChunk) || result.Offset != 250 {
		t.Fatalf("short piece: %+v %v", result, err)
	}
	if offset, _, _ := f.store.Offset(shaOf(data)); offset != 250 {
		t.Fatalf("offset after short piece = %d", offset)
	}
	if result, err := f.chunk(data, 250, int64(len(data))); err != nil || result.Item == nil {
		t.Fatalf("finish = %+v %v", result, err)
	}
}

func TestUploadWithWrongBytesIsDiscarded(t *testing.T) {
	f := newFixture(t)
	data := []byte("the real photo")
	f.start(photoRequest(data, "IMG.jpg"))
	tampered := []byte("the fake photo")
	_, err := f.store.WriteChunk(shaOf(data), 0, int64(len(data)), bytes.NewReader(tampered), int64(len(tampered)))
	if !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := f.store.Offset(shaOf(data)); !errors.Is(err, ErrNoUpload) {
		t.Fatalf("upload still there: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.root, "media")); len(entries) != 0 {
		t.Fatalf("something was stored: %v", entries)
	}
}

func TestSameNameGetsANumberIgnoringCase(t *testing.T) {
	f := newFixture(t)
	first := f.upload([]byte("one"), "IMG_0001.jpg")
	second := f.upload([]byte("two"), "img_0001.JPG")
	third := f.upload([]byte("three"), "IMG_0001.jpg")
	if first.Path != "media/2026/2026-10/IMG_0001.jpg" ||
		second.Path != "media/2026/2026-10/img_0001 (2).JPG" ||
		third.Path != "media/2026/2026-10/IMG_0001 (3).jpg" {
		t.Fatalf("paths = %s, %s, %s", first.Path, second.Path, third.Path)
	}
}

func TestUndatedFilesAndVersions(t *testing.T) {
	f := newFixture(t)
	original := f.upload([]byte("original"), "IMG.jpg")

	edit := []byte("edited")
	req := photoRequest(edit, "IMG.jpg")
	req.TakenAt = nil
	unknown := "0000000000000000000000000000000000000000000000000000000000000000"
	req.VersionOf = &unknown
	if result := f.start(req); result.Rejected != RejectUnknownOriginal {
		t.Fatalf("unknown original = %+v", result)
	}
	sha := original.Sha256()
	req.VersionOf = &sha
	f.start(req)
	result, err := f.chunk(edit, 0, int64(len(edit)))
	if err != nil || result.Item.Path != "media/undated/IMG.jpg" {
		t.Fatalf("edit = %+v %v", result, err)
	}
	if versions, paired := f.store.Related(sha); len(versions) != 1 || versions[0] != shaOf(edit) || len(paired) != 0 {
		t.Fatalf("related = %v %v", versions, paired)
	}
}

func TestInvalidRequestsAndLimits(t *testing.T) {
	f := newFixture(t)
	data := []byte("x")
	for name, change := range map[string]func(*UploadRequest){
		"uppercase sha":  func(r *UploadRequest) { r.Sha256 = "AB" + r.Sha256[2:] },
		"empty name":     func(r *UploadRequest) { r.FileName = " " },
		"no size":        func(r *UploadRequest) { r.Size = 0 },
		"bad kind":       func(r *UploadRequest) { r.Kind = "audio" },
		"self version":   func(r *UploadRequest) { r.VersionOf = &r.Sha256 },
		"header in mime": func(r *UploadRequest) { r.MimeType = "image/jpeg\r\nX-Injected: 1" },
		"no subtype":     func(r *UploadRequest) { r.MimeType = "image" },
	} {
		req := photoRequest(data, "a.jpg")
		change(&req)
		var invalid *InvalidRequestError
		if _, err := f.store.StartUpload(req); !errors.As(err, &invalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	big := photoRequest(data, "huge.mov")
	big.Size = MaxFileSize + 1
	if result := f.start(big); result.Rejected != RejectTooLarge {
		t.Fatalf("too large = %+v", result)
	}
}

func TestExpiredUploadsArePurged(t *testing.T) {
	f := newFixture(t)
	data := []byte("left halfway")
	f.start(photoRequest(data, "IMG.jpg"))
	f.chunk(data, 0, 4)
	stray := filepath.Join(f.store.uploadsDir(), shaOf([]byte("stray"))+".partial")
	os.WriteFile(stray, []byte("st"), 0o644)

	f.store.PurgeExpiredUploads()
	if _, _, err := f.store.Offset(shaOf(data)); err != nil {
		t.Fatalf("fresh upload purged: %v", err)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatalf("piece without a session kept")
	}
	f.now = f.now.Add(UploadLifetime + time.Minute)
	f.store.PurgeExpiredUploads()
	if entries, _ := os.ReadDir(f.store.uploadsDir()); len(entries) != 0 {
		t.Fatalf("expired upload kept: %v", entries)
	}
}

func TestSafeFileName(t *testing.T) {
	for in, want := range map[string]string{
		"IMG_1234.HEIC":          "IMG_1234.HEIC",
		`DCIM\Camera\a.jpg`:      "a.jpg",
		"../../etc/passwd":       "passwd",
		`what: "now"?.jpg`:       "what_ _now__.jpg",
		"CON.jpg":                "_CON.jpg",
		"nul":                    "_nul",
		"trailing. ":             "trailing",
		"":                       "file",
		".hidden":                ".hidden",
		"שלום.jpg":               "שלום.jpg",
		"tab\there.png":          "tab_here.png",
		"many.dots.in.name.webp": "many.dots.in.name.webp",
		"IMG[page=1].jpg":        "IMG_page=1_.jpg",
	} {
		if got := safeFileName(in); got != want {
			t.Errorf("safeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestChangesPageThroughEverythingInUploadOrder(t *testing.T) {
	f := newFixture(t)
	var uploaded []string
	for i := range 5 {
		f.now = f.now.Add(time.Second)
		uploaded = append(uploaded, f.upload([]byte{byte(i), 'x'}, "IMG.jpg").Sha256())
	}
	var seen []string
	cursor := ""
	for {
		items, next, remaining, err := f.store.Changes(cursor, 2, View{})
		if err != nil {
			t.Fatal(err)
		}
		if want := len(uploaded) - len(seen) - len(items); remaining != want {
			t.Fatalf("remaining %d, want %d", remaining, want)
		}
		for _, item := range items {
			seen = append(seen, item.Sha256())
		}
		cursor = next
		if remaining == 0 {
			break
		}
	}
	if strings.Join(seen, ",") != strings.Join(uploaded, ",") {
		t.Fatalf("seen %v, want %v", seen, uploaded)
	}

	// Nothing new: the same cursor comes back. A new upload shows up after it.
	if items, next, remaining, _ := f.store.Changes(cursor, 2, View{}); len(items) != 0 || next != cursor || remaining != 0 {
		t.Fatalf("after the end: %v %q %d", items, next, remaining)
	}
	f.now = f.now.Add(time.Second)
	latest := f.upload([]byte("new"), "IMG.jpg")
	if items, _, _, _ := f.store.Changes(cursor, 10, View{}); len(items) != 1 || items[0].Sha256() != latest.Sha256() {
		t.Fatalf("new upload: %v", items)
	}
	// A cursor the server can't read starts the feed again: the device may hold anything.
	if items, next, _, err := f.store.Changes("garbage", 10, View{}); err != nil || len(items) != 6 || next == "" {
		t.Fatalf("bad cursor: %d items, %q, %v", len(items), next, err)
	}
}

// A piece that ends early keeps what arrived, and the next piece goes on from there.
func TestTornPieceKeepsWhatArrived(t *testing.T) {
	f := newFixture(t)
	data := []byte("0123456789")
	f.start(photoRequest(data, "IMG.jpg"))
	result, err := f.store.WriteChunk(shaOf(data), 0, 10, bytes.NewReader(data[:4]), 6)
	if !errors.Is(err, ErrIncompleteChunk) || result.Offset != 4 {
		t.Fatalf("torn piece: %+v %v", result, err)
	}
	if offset, complete, err := f.store.Offset(shaOf(data)); offset != 4 || complete || err != nil {
		t.Fatalf("offset after a torn piece: %d %v %v", offset, complete, err)
	}
	if result, err := f.chunk(data, 4, 10); err != nil || result.Item == nil {
		t.Fatalf("going on: %+v %v", result, err)
	}
}

func TestSetNudityKeepsTheSidecarAndComesAgainInTheFeed(t *testing.T) {
	f := newFixture(t)
	f.now = f.now.Add(time.Second)
	item := f.upload([]byte("photo"), "IMG.jpg")
	_, cursor, _, _ := f.store.Changes("", 10, View{})

	// Scanned an hour later; the time is the store's, taken during the write's turn.
	f.now = f.now.Add(time.Hour)
	scanned := f.now
	if err := f.store.SetNudity(item.Sha256(), Detection{Score: 0.93, Model: "nudenet-320n", ScannedAt: scanned.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	items, _, _, _ := f.store.Changes(cursor, 10, View{})
	if len(items) != 1 || items[0].Sidecar.Detected.Nudity.Score != 0.93 {
		t.Fatalf("after the scan: %v", items)
	}

	// The answer is in the file: a store opened again reads it, with the facts as they were.
	again, err := OpenStore(f.store.Root(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := again.Get(item.Sha256())
	if d := got.Sidecar.Detected; d == nil || d.Nudity.Model != "nudenet-320n" || !d.Nudity.ScannedAt.Equal(scanned) || got.Sidecar.Facts.FileName != "IMG.jpg" {
		t.Fatalf("reopened: %+v", got.Sidecar)
	}
}

func TestSetSourceComesAgainInTheFeed(t *testing.T) {
	f := newFixture(t)
	f.now = f.now.Add(time.Second)
	item := f.upload([]byte("photo"), "IMG-20230115-WA0012.jpg")
	_, cursor, _, _ := f.store.Changes("", 10, View{})

	f.now = f.now.Add(time.Hour)
	if changed, err := f.store.SetSource(item.Sha256(), SourceWhatsApp); err != nil || !changed {
		t.Fatal(changed, err)
	}
	items, cursor, _, _ := f.store.Changes(cursor, 10, View{})
	if len(items) != 1 || *items[0].Sidecar.Facts.Source != SourceWhatsApp {
		t.Fatalf("after the correction: %v", items)
	}

	// The same source again changes nothing, and the feed has nothing new.
	f.now = f.now.Add(time.Hour)
	if changed, err := f.store.SetSource(item.Sha256(), SourceWhatsApp); err != nil || changed {
		t.Fatal(changed, err)
	}
	if items, _, _, _ := f.store.Changes(cursor, 10, View{}); len(items) != 0 {
		t.Fatalf("unchanged: %v", items)
	}

	again, err := OpenStore(f.store.Root(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := again.Get(item.Sha256())
	if c := got.Sidecar.Facts.CorrectedAt; c == nil || *got.Sidecar.Facts.Source != SourceWhatsApp || got.Sidecar.Facts.FileName != "IMG-20230115-WA0012.jpg" {
		t.Fatalf("reopened: %+v", got.Sidecar.Facts)
	}
}

func TestScreenshotsGetTheirApp(t *testing.T) {
	f := newFixture(t)
	f.now = f.now.Add(time.Second)

	// A new upload gets them from its name, whatever source the device sent.
	req := photoRequest([]byte("bumble"), "Screenshot_20240301_030715_Bumble.jpg")
	camera := SourceCamera
	req.Source = &camera
	f.start(req)
	result, err := f.chunk([]byte("bumble"), 0, 6)
	if err != nil || result.Item == nil {
		t.Fatal(result, err)
	}
	if facts := result.Item.Sidecar.Facts; *facts.Source != SourceScreenshots || facts.App == nil || *facts.App != "Bumble" {
		t.Fatalf("uploaded: %+v", facts)
	}

	// A file placed before the server read the name gets them from SetScreenshotFacts, and comes again in the feed.
	item := f.upload([]byte("tinder"), "Screenshot_20240228-080750_Tinder(1).jpg")
	old := item.Sidecar
	old.Facts.Source, old.Facts.App = &camera, nil
	f.store.mu.Lock()
	err = f.store.rewriteSidecar(f.store.items[item.Sha256()], old)
	f.store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	_, cursor, _, _ := f.store.Changes("", 10, View{})
	f.now = f.now.Add(time.Hour)
	if changed, err := f.store.SetScreenshotFacts(item.Sha256()); err != nil || !changed {
		t.Fatal(changed, err)
	}
	items, cursor, _, _ := f.store.Changes(cursor, 10, View{})
	if len(items) != 1 || *items[0].Sidecar.Facts.Source != SourceScreenshots || *items[0].Sidecar.Facts.App != "Tinder" {
		t.Fatalf("after the correction: %v", items)
	}
	f.now = f.now.Add(time.Hour)
	if changed, err := f.store.SetScreenshotFacts(item.Sha256()); err != nil || changed {
		t.Fatal(changed, err)
	}
	if items, _, _, _ := f.store.Changes(cursor, 10, View{}); len(items) != 0 {
		t.Fatalf("unchanged: %v", items)
	}

	// Other files are left as they are.
	photo := f.upload([]byte("photo"), "20251101_110819.heic")
	if changed, err := f.store.SetScreenshotFacts(photo.Sha256()); err != nil || changed || photo.Sidecar.Facts.App != nil {
		t.Fatal(changed, err, photo.Sidecar.Facts)
	}
}
