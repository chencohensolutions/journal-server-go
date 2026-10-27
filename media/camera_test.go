package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cameraTIFF is a TIFF header and IFD0 naming a camera's make and model, as EXIF and DNGs carry them.
func cameraTIFF(maker, model string) []byte {
	var b bytes.Buffer
	b.WriteString("MM\x00\x2a")
	binary.Write(&b, binary.BigEndian, uint32(8))
	binary.Write(&b, binary.BigEndian, uint16(2))
	strs := []string{maker + "\x00", model + "\x00"}
	offset := uint32(8 + 2 + 2*12 + 4)
	for i, tag := range []uint16{0x010F, 0x0110} {
		binary.Write(&b, binary.BigEndian, tag)
		binary.Write(&b, binary.BigEndian, uint16(2)) // ASCII
		binary.Write(&b, binary.BigEndian, uint32(len(strs[i])))
		binary.Write(&b, binary.BigEndian, offset)
		offset += uint32(len(strs[i]))
	}
	binary.Write(&b, binary.BigEndian, uint32(0)) // no next IFD
	for _, s := range strs {
		b.WriteString(s)
	}
	return b.Bytes()
}

// cameraJPEG is a small JPEG whose EXIF names a camera.
func cameraJPEG(t *testing.T, maker, model string) []byte {
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewGray(image.Rect(0, 0, 16, 16)), nil); err != nil {
		t.Fatal(err)
	}
	tiff := cameraTIFF(maker, model)
	app1 := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(app1[2:], uint16(2+6+len(tiff)))
	app1 = append(append(app1, "Exif\x00\x00"...), tiff...)
	data := encoded.Bytes()
	return append(append(append([]byte{}, data[:2]...), app1...), data[2:]...)
}

func writeCameraFile(t *testing.T, name string, data []byte) string {
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func wantCamera(t *testing.T, got *Camera, maker, model string) {
	t.Helper()
	if got == nil || got.Make != maker || got.Model != model {
		t.Fatalf("camera %+v, want %s %s", got, maker, model)
	}
}

func TestReadCameraOfADNG(t *testing.T) {
	path := writeCameraFile(t, "x.dng", cameraTIFF("samsung", "SM-S931B"))
	wantCamera(t, ReadCamera(context.Background(), CameraTools{}, path, KindPhoto), "samsung", "SM-S931B")
	if c := ReadCamera(context.Background(), CameraTools{}, writeCameraFile(t, "y.jpg", []byte("not a photo")), KindPhoto); c != nil {
		t.Fatalf("no tools, not a TIFF: %+v", c)
	}
}

func TestReadCameraInTheServer(t *testing.T) {
	lib := loadTestLib(t)
	path := writeCameraFile(t, "x.jpg", cameraJPEG(t, "Canon", "Canon EOS 80D"))
	wantCamera(t, ReadCamera(context.Background(), CameraTools{lib: lib}, path, KindPhoto), "Canon", "Canon EOS 80D")
	plain := writeCameraFile(t, "plain.jpg", orientedJPEG(t))
	if c := ReadCamera(context.Background(), CameraTools{lib: lib}, plain, KindPhoto); c != nil {
		t.Fatalf("a photo without a camera: %+v", c)
	}
}

func TestReadCameraWithVipsheader(t *testing.T) {
	_, header, err := FindVips(filepath.Join("..", "vips", "bin", "vips"))
	if err != nil {
		if _, header, err = FindVips(""); err != nil {
			t.Skip("libvips not installed")
		}
	}
	path := writeCameraFile(t, "x.jpg", cameraJPEG(t, "Canon", "Canon EOS 80D"))
	wantCamera(t, ReadCamera(context.Background(), CameraTools{vipsheader: header}, path, KindPhoto), "Canon", "Canon EOS 80D")
}

func TestReadCameraOfAVideo(t *testing.T) {
	ffmpeg, ffprobe, err := FindFFmpeg("")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	video := filepath.Join(t.TempDir(), "x.mp4")
	if out, err := exec.Command(ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c=black:s=16x16:d=0.2", "-pix_fmt", "yuv420p",
		"-metadata", "com.android.manufacturer=samsung", "-metadata", "com.android.model=SM-S931B",
		"-movflags", "use_metadata_tags", video).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg can't make the test video: %v %s", err, out)
	}
	wantCamera(t, ReadCamera(context.Background(), CameraTools{ffprobe: ffprobe}, video, KindVideo), "samsung", "SM-S931B")
	if c := CameraFromTags(map[string]string{"com.apple.quicktime.make": "Apple", "com.apple.quicktime.model": "iPhone 15 Pro"}); c == nil || c.Model != "iPhone 15 Pro" {
		t.Fatalf("QuickTime's tags: %+v", c)
	}
}

// A photo's camera is read when it arrives; marking its model not mine moves its files to received in the
// background, a correction each, and marking it mine again brings them back.
func TestMarkingACameraMovesItsFiles(t *testing.T) {
	f := newFixture(t)
	upload := func(data []byte, name string) Item {
		req := photoRequest(data, name)
		req.MimeType, req.Source = "image/x-adobe-dng", ptr(SourceCamera)
		f.start(req)
		result, err := f.chunk(data, 0, int64(len(data)))
		if err != nil || result.Item == nil {
			t.Fatal(result, err)
		}
		return *result.Item
	}
	canon1 := upload(cameraTIFF("Canon", "Canon EOS 80D"), "a.dng")
	canon2 := upload(append(cameraTIFF("Canon", "Canon EOS 80D"), 1), "b.dng")
	mine := upload(cameraTIFF("samsung", "SM-S931B"), "c.dng")
	wantCamera(t, canon1.Sidecar.Facts.Camera, "Canon", "Canon EOS 80D")

	models := f.store.Cameras()
	if len(models) != 2 || models[0].Model != "Canon EOS 80D" || models[0].Files != 2 || !models[0].Mine || models[0].Marked ||
		len(models[0].Years) != 1 || models[0].Years[0] != 2026 {
		t.Fatalf("cameras: %+v", models)
	}

	f.now = f.now.Add(time.Hour)
	if err := f.store.SetCamera("canon", "CANON EOS 80D", false); err != nil {
		t.Fatal(err)
	}
	f.store.WaitCameraMoves()
	for _, sha := range []string{canon1.Sha256(), canon2.Sha256()} {
		facts := f.store.mustGet(t, sha).Sidecar.Facts
		if *facts.Source != SourceReceived || *facts.SentFrom != SourceCamera || facts.CorrectedAt == nil || !facts.CorrectedAt.Equal(f.now) {
			t.Fatalf("not moved: %+v", facts)
		}
	}
	if facts := f.store.mustGet(t, mine.Sha256()).Sidecar.Facts; *facts.Source != SourceCamera || facts.CorrectedAt != nil {
		t.Fatalf("another camera moved: %+v", facts)
	}
	models = f.store.Cameras()
	if models[0].Mine || !models[0].Marked || models[0].Pending != 0 {
		t.Fatalf("after the mark: %+v", models[0])
	}
	var file camerasFile
	data, _ := os.ReadFile(filepath.Join(f.root, "cameras.json"))
	if json.Unmarshal(data, &file) != nil || file.Format != 1 || len(file.Cameras) != 1 || file.Cameras[0].Mine {
		t.Fatalf("cameras.json: %s", data)
	}

	// A new photo of that camera arrives as received; the marks outlive a restart.
	f.reopen()
	if got := upload(append(cameraTIFF("Canon", "Canon EOS 80D"), 2), "d.dng"); *got.Sidecar.Facts.Source != SourceReceived {
		t.Fatalf("new photo: %s", *got.Sidecar.Facts.Source)
	}

	f.now = f.now.Add(time.Hour)
	if err := f.store.SetCamera("Canon", "Canon EOS 80D", true); err != nil {
		t.Fatal(err)
	}
	f.store.WaitCameraMoves()
	if facts := f.store.mustGet(t, canon1.Sha256()).Sidecar.Facts; *facts.Source != SourceCamera || !facts.CorrectedAt.Equal(f.now) {
		t.Fatalf("not back: %+v", facts)
	}
	if err := f.store.SetCamera("", " ", false); err == nil {
		t.Fatal("a camera without a make or model was marked")
	}
}

// The background move says how many files are left (pending) while it runs.
func TestCameraMovesCountWhatIsPending(t *testing.T) {
	f := newFixture(t)
	for i := range 3 {
		data := append(cameraTIFF("NIKON CORPORATION", "NIKON D90"), byte(i))
		f.start(photoRequest(data, "x.dng"))
		if _, err := f.chunk(data, 0, int64(len(data))); err != nil {
			t.Fatal(err)
		}
	}
	// Another move runs, so this one waits with its files counted.
	f.store.cameraMoves.run.Lock()
	if err := f.store.SetCamera("NIKON CORPORATION", "NIKON D90", false); err != nil {
		t.Fatal(err)
	}
	if models := f.store.Cameras(); models[0].Pending != 3 {
		t.Fatalf("pending %d, want 3: %+v", models[0].Pending, models)
	}
	f.store.cameraMoves.run.Unlock()
	f.store.WaitCameraMoves()
	if models := f.store.Cameras(); models[0].Pending != 0 {
		t.Fatalf("pending %d after the move", models[0].Pending)
	}
}

// A sidecar written before Oct 27, 2026: the screenshot's app under app, WhatsApp as whatsapp. Both are read as now,
// and the next rewrite writes them so, keeping what was sent in sentFrom.
func TestOldSidecarsAreReadAsNow(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "media", "2024", "2024-03")
	os.MkdirAll(dir, 0o755)
	write := func(name, facts string) string {
		data := []byte(name)
		sha := shaOf(data)
		os.WriteFile(filepath.Join(dir, name), data, 0o644)
		sidecar := `{"format":1,"sha256":"` + sha + `","facts":{"fileName":"` + name + `","kind":"photo","mimeType":"image/jpeg","size":` +
			`1,"uploadedAt":"2026-10-20T17:50:53Z","uploadedBy":"import",` + facts + `}}`
		os.WriteFile(filepath.Join(dir, name+".json"), []byte(sidecar), 0o644)
		return sha
	}
	shot := write("Screenshot_20240301_030715_Bumble.jpg", `"source":"screenshots","app":"Bumble"`)
	wa := write("IMG-20240301-WA0001.jpg", `"source":"whatsapp"`)
	// Folder kinds as phones sent them before ids were in lower case: read in lower case, as sent in sentFrom.
	wa2 := write("x.jpg", `"source":"folder:WhatsApp"`)
	calls := write("y.jpg", `"source":"folder:Calls","sentFrom":"folder:Calls"`)
	store, err := OpenStore(root, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if facts := store.mustGet(t, wa2).Sidecar.Facts; *facts.Source != SourceWhatsApp || *facts.SentFrom != "folder:WhatsApp" {
		t.Fatalf("folder:WhatsApp: %+v", facts)
	}
	if facts := store.mustGet(t, calls).Sidecar.Facts; *facts.Source != "folder:calls" || *facts.SentFrom != "folder:Calls" {
		t.Fatalf("folder:Calls: %+v", facts)
	}
	if names := store.FolderNames(); len(names) != 2 || names[0] != (FolderKindName{"folder:calls", "Calls"}) || names[1] != (FolderKindName{SourceWhatsApp, "WhatsApp"}) {
		t.Fatalf("folder names: %+v", names)
	}
	if facts := store.mustGet(t, shot).Sidecar.Facts; facts.ScreenshotApp == nil || *facts.ScreenshotApp != "Bumble" {
		t.Fatalf("screenshot: %+v", facts)
	}
	facts := store.mustGet(t, wa).Sidecar.Facts
	if *facts.Source != SourceWhatsApp || *facts.SentFrom != OldSourceWhatsApp {
		t.Fatalf("whatsapp: %+v", facts)
	}
	if _, err := store.Relabel(shot, SourceScreenshots, ptr("Tinder")); err != nil {
		t.Fatal(err)
	}
	if err := store.SetNudity(wa, Detection{Score: 0.1, Model: "test"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "Screenshot_20240301_030715_Bumble.jpg.json"))
	if !strings.Contains(string(data), `"screenshotApp": "Tinder"`) || strings.Contains(string(data), `"app"`) {
		t.Fatalf("rewritten screenshot: %s", data)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "IMG-20240301-WA0001.jpg.json"))
	if !strings.Contains(string(data), `"source": "folder:whatsapp"`) || !strings.Contains(string(data), `"sentFrom": "whatsapp"`) {
		t.Fatalf("rewritten WhatsApp file: %s", data)
	}
}
