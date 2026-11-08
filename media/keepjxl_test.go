package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func jpegRequest(data []byte, name string) UploadRequest {
	req := photoRequest(data, name)
	req.MimeType = "image/jpeg"
	return req
}

// uploadJPEG uploads data as a JPEG photo and returns what was stored.
func (f *fixture) uploadJPEG(data []byte, name string) Item {
	f.t.Helper()
	if r := f.start(jpegRequest(data, name)); r.Upload == nil {
		f.t.Fatalf("start %s = %+v", name, r)
	}
	result, err := f.chunk(data, 0, int64(len(data)))
	if err != nil || result.Item == nil {
		f.t.Fatalf("upload %s: %+v %v", name, result, err)
	}
	return *result.Item
}

func TestKeepAsJXLReplacesTheJPEG(t *testing.T) {
	x := testJXL(t)
	f := newFixture(t)
	jpeg := withExif(photoJPEG(t, 320, 240, 92), "samsung", "SM-S931B")
	old := f.uploadJPEG(jpeg, "IMG_1234.jpg") // no libjxl yet: kept as it came
	if old.Sidecar.Facts.MimeType != "image/jpeg" {
		t.Fatalf("stored as %s", old.Sidecar.Facts.MimeType)
	}
	edit := photoRequest([]byte("an edit of the photo"), "IMG_1234_edit.heic")
	edit.VersionOf = &old.Sidecar.Sha256
	f.start(edit)
	if _, err := f.chunk([]byte("an edit of the photo"), 0, 20); err != nil {
		t.Fatal(err)
	}
	other, err := OpenStore(f.root, time.Now) // a second process with the folder open, such as journal-server import
	if err != nil {
		t.Fatal(err)
	}

	f.now = f.now.Add(time.Hour)
	f.store.JXL = x
	kept, err := f.store.KeepAsJXL(old.Sha256())
	if err != nil {
		t.Fatal(err)
	}
	facts := kept.Sidecar.Facts
	if kept.Path != "media/2026/2026-10/IMG_1234.jxl" || facts.FileName != "IMG_1234.jxl" || facts.MimeType != "image/jxl" ||
		facts.Size >= int64(len(jpeg)) || kept.Sha256() == old.Sha256() {
		t.Fatalf("kept as %s %+v", kept.Path, facts)
	}
	from := facts.EncodedFrom
	if from == nil || *from != (EncodedFrom{Sha256: old.Sha256(), MimeType: "image/jpeg", Size: int64(len(jpeg)), FileName: "IMG_1234.jpg"}) {
		t.Fatalf("encodedFrom %+v", from)
	}
	if facts.CorrectedAt == nil || !facts.CorrectedAt.Equal(f.now) || !facts.UploadedAt.Equal(old.Sidecar.Facts.UploadedAt) {
		t.Errorf("correctedAt %v, uploadedAt %v", facts.CorrectedAt, facts.UploadedAt)
	}
	if _, err := os.Stat(filepath.Join(f.root, filepath.FromSlash(old.Path))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the JPEG is still there: %v", err)
	}
	if _, ok := f.store.Get(old.Sha256()); ok {
		t.Error("the JPEG is still indexed")
	}
	if found, ok := f.store.Find(old.Sha256()); !ok || found.Sha256() != kept.Sha256() {
		t.Errorf("the JPEG's sha256 finds %+v %v", found, ok)
	}
	if missing := f.store.Missing([]string{old.Sha256(), kept.Sha256()}); len(missing) != 0 {
		t.Errorf("missing %v", missing)
	}
	if again, err := f.store.JPEGOf(kept); err != nil || !bytes.Equal(again, jpeg) {
		t.Errorf("the JPEG made again differs: %v", err)
	}
	for _, sha := range []string{old.Sha256(), kept.Sha256()} {
		if v, err := f.store.Verify(context.Background(), sha); v != VerifyOK || err != nil {
			t.Errorf("verify %s = %s %v", sha, v, err)
		}
	}
	if e, _ := f.store.Get(shaOf([]byte("an edit of the photo"))); e.Sidecar.Facts.VersionOf == nil || *e.Sidecar.Facts.VersionOf != kept.Sha256() {
		t.Errorf("the edit is a version of %v", e.Sidecar.Facts.VersionOf)
	}
	if _, err := f.store.KeepAsJXL(kept.Sha256()); !errors.Is(err, ErrNotJPEG) {
		t.Errorf("keeping the JPEG XL again: %v", err)
	}

	// The other process learns of it at its next turn or question, and so does a store opened afresh.
	other.CatchUp()
	f.reopen()
	for name, s := range map[string]*Store{"other": other, "reopened": f.store} {
		if _, ok := s.Get(old.Sha256()); ok {
			t.Errorf("%s: the JPEG is still indexed", name)
		}
		if found, ok := s.Find(old.Sha256()); !ok || found.Sha256() != kept.Sha256() {
			t.Errorf("%s: the JPEG's sha256 finds %+v %v", name, found, ok)
		}
	}
}

func TestAJPEGLeftBesideItsJXLIsLeftOut(t *testing.T) {
	x := testJXL(t)
	f := newFixture(t)
	jpeg := photoJPEG(t, 200, 100, 85)
	old := f.uploadJPEG(jpeg, "IMG_1.jpg")
	sidecar, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(old.Path)) + ".json")
	if err != nil {
		t.Fatal(err)
	}
	f.store.JXL = x
	kept, err := f.store.KeepAsJXL(old.Sha256())
	if err != nil {
		t.Fatal(err)
	}
	// As if the server stopped before deleting the JPEG.
	path := filepath.Join(f.root, filepath.FromSlash(old.Path))
	os.WriteFile(path, jpeg, 0o644)
	os.WriteFile(path+".json", sidecar, 0o644)
	f.reopen()
	if _, ok := f.store.Get(old.Sha256()); ok {
		t.Error("the leftover JPEG is indexed")
	}
	if found, ok := f.store.Find(old.Sha256()); !ok || found.Sha256() != kept.Sha256() {
		t.Errorf("the JPEG's sha256 finds %+v %v", found, ok)
	}
}

// TestAStoppedReplacementIsFinishedOrUndone stops KeepAsJXL's turn at each of its steps (the files as a killed process
// leaves them, with its record) and opens the data folder again: before the JPEG XL's sidecar, the replacement is
// undone and the JPEG stays; after it, the replacement is finished and the JPEG goes. No spare copy is left either way.
func TestAStoppedReplacementIsFinishedOrUndone(t *testing.T) {
	x := testJXL(t)
	f := newFixture(t)
	jpeg := photoJPEG(t, 200, 100, 85)
	old := f.uploadJPEG(jpeg, "IMG_1.jpg")
	jpegPath := filepath.Join(f.root, filepath.FromSlash(old.Path))
	jpegSidecar, err := os.ReadFile(jpegPath + ".json")
	if err != nil {
		t.Fatal(err)
	}
	f.store.JXL = x
	kept, err := f.store.KeepAsJXL(old.Sha256())
	if err != nil {
		t.Fatal(err)
	}
	jxlPath := filepath.Join(f.root, filepath.FromSlash(kept.Path))
	jxl, _ := os.ReadFile(jxlPath)
	jxlSidecar, _ := os.ReadFile(jxlPath + ".json")
	record := filepath.Join(f.root, ".cache", "jxl", "replacing", old.Sha256()+".json")
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("the record outlived the replacement: %v", err)
	}

	exists := func(path string) bool { _, err := os.Stat(path); return err == nil }
	stopped := func(settled bool, files map[string][]byte) {
		t.Helper()
		for _, p := range []string{jpegPath, jpegPath + ".json", jxlPath, jxlPath + ".json"} {
			os.Remove(p)
		}
		for p, data := range files {
			if err := os.WriteFile(p, data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		os.MkdirAll(filepath.Dir(record), 0o755)
		data, _ := json.Marshal(replacing{JPEG: old.Path, JPEGSha: old.Sha256(), JXL: kept.Path, JXLSha: kept.Sha256()})
		os.WriteFile(record, data, 0o644)
		os.WriteFile(filepath.Join(filepath.Dir(jxlPath), "."+filepath.Base(jxlPath)+".json.123.tmp"), []byte("{"), 0o644)
		os.Remove(filepath.Join(f.root, ".cache", "index.json")) // the folder is read again, as after any change
		f.reopen()
		if exists(record) == settled {
			t.Errorf("the record is there: %v; settled: %v", exists(record), settled)
		}
		if temps, _ := filepath.Glob(filepath.Join(filepath.Dir(jxlPath), ".*.tmp")); len(temps) > 0 {
			t.Errorf("half-written files left: %v", temps)
		}
	}
	undone := func(stage string) {
		t.Helper()
		if exists(jxlPath) || exists(jxlPath+".json") || !exists(jpegPath) || !exists(jpegPath+".json") {
			t.Errorf("%s: not undone", stage)
		}
		if got, ok := f.store.Get(old.Sha256()); !ok || got.Path != old.Path {
			t.Errorf("%s: the JPEG isn't in the index: %+v", stage, got)
		}
	}
	finished := func(stage string) {
		t.Helper()
		if exists(jpegPath) || exists(jpegPath+".json") || !exists(jxlPath) || !exists(jxlPath+".json") {
			t.Errorf("%s: not finished", stage)
		}
		if found, ok := f.store.Find(old.Sha256()); !ok || found.Sha256() != kept.Sha256() {
			t.Errorf("%s: the JPEG's sha256 finds %+v", stage, found)
		}
	}

	stopped(true, map[string][]byte{jpegPath: jpeg, jpegPath + ".json": jpegSidecar}) // before the JPEG XL was placed
	undone("before the JPEG XL")
	stopped(true, map[string][]byte{jpegPath: jpeg, jpegPath + ".json": jpegSidecar, jxlPath: jxl}) // before its sidecar
	undone("before the JPEG XL's sidecar")
	stopped(true, map[string][]byte{jpegPath: jpeg, jpegPath + ".json": jpegSidecar, jxlPath: jxl, jxlPath + ".json": jxlSidecar})
	finished("before the JPEG's sidecar went")
	stopped(true, map[string][]byte{jpegPath: jpeg, jxlPath: jxl, jxlPath + ".json": jxlSidecar})
	finished("before the JPEG went")

	// A JPEG XL that isn't whole is never trusted: the JPEG stays, and so does the record, for the next time.
	stopped(false, map[string][]byte{jpegPath: jpeg, jxlPath: jxl[:len(jxl)/2], jxlPath + ".json": jxlSidecar})
	if !exists(jpegPath) {
		t.Error("the JPEG went though its JPEG XL isn't whole")
	}
}

func TestUploadedJPEGIsKeptAsJXL(t *testing.T) {
	x := testJXL(t)
	f := newFixture(t)
	f.store.JXL = x
	jpeg := photoJPEG(t, 320, 240, 90)
	kept := f.uploadJPEG(jpeg, "20261017_155222.jpg")
	facts := kept.Sidecar.Facts
	if facts.MimeType != "image/jxl" || facts.FileName != "20261017_155222.jxl" || facts.EncodedFrom == nil ||
		facts.EncodedFrom.Sha256 != shaOf(jpeg) || facts.EncodedFrom.FileName != "20261017_155222.jpg" {
		t.Fatalf("stored as %s %+v", kept.Path, facts)
	}
	if facts.CorrectedAt != nil {
		t.Errorf("an upload has no correction: %v", facts.CorrectedAt)
	}
	// The same JPEG from another device is there already, and so are its bytes for a retried piece.
	if r := f.start(jpegRequest(jpeg, "copy.jpg")); r.Existing == nil || r.Existing.Sha256() != kept.Sha256() {
		t.Errorf("starting the JPEG again = %+v", r)
	}
	if offset, complete, err := f.store.Offset(shaOf(jpeg)); !complete || offset != int64(len(jpeg)) || err != nil {
		t.Errorf("offset %d complete %v %v", offset, complete, err)
	}
	if entries, _ := os.ReadDir(f.store.uploadsDir()); len(entries) != 0 {
		t.Errorf("left in uploads: %v", entries)
	}

	// A JPEG libjxl can't keep stays a JPEG.
	gray := cameraJPEG(t, "Canon", "Canon EOS 80D")
	if item := f.uploadJPEG(gray, "IMG_0001.JPG"); item.Sha256() != shaOf(gray) || item.Sidecar.Facts.EncodedFrom != nil {
		t.Errorf("stored as %+v", item.Sidecar.Facts)
	}
}

func TestDeviceJXLOfAJPEGIsChecked(t *testing.T) {
	x := testJXL(t)
	f := newFixture(t)
	f.store.JXL = x
	jpeg := photoJPEG(t, 320, 240, 90)
	jxl, err := x.Keep(jpeg)
	if err != nil {
		t.Fatal(err)
	}
	req := func(from EncodedFrom) UploadRequest {
		r := photoRequest(jxl, "IMG_5.jxl")
		r.MimeType = "image/jxl"
		r.EncodedFrom = &from
		return r
	}
	wrong := req(EncodedFrom{Sha256: shaOf([]byte("another jpeg")), MimeType: "image/jpeg", Size: int64(len(jpeg)), FileName: "IMG_5.jpg"})
	f.start(wrong)
	if _, err := f.chunk(jxl, 0, int64(len(jxl))); !errors.Is(err, ErrNotTheJPEG) {
		t.Fatalf("a JPEG XL naming another JPEG: %v", err)
	}
	if _, ok := f.store.Get(shaOf(jxl)); ok {
		t.Fatal("placed")
	}
	f.start(req(EncodedFrom{Sha256: shaOf(jpeg), MimeType: "image/jpeg", Size: int64(len(jpeg)), FileName: "IMG_5.jpg"}))
	result, err := f.chunk(jxl, 0, int64(len(jxl)))
	if err != nil || result.Item == nil || result.Item.Sha256() != shaOf(jxl) {
		t.Fatalf("upload = %+v %v", result, err)
	}
	if found, ok := f.store.Find(shaOf(jpeg)); !ok || found.Sha256() != shaOf(jxl) {
		t.Errorf("the JPEG's sha256 finds %+v %v", found, ok)
	}
	if v, err := f.store.Verify(context.Background(), shaOf(jpeg)); v != VerifyOK || err != nil {
		t.Errorf("verify the JPEG = %s %v", v, err)
	}
}
