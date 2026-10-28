package media

import (
	"bytes"
	"context"
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
	other, err := OpenStore(f.root, time.Now) // a second process with the folder open, such as scripts/import
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
