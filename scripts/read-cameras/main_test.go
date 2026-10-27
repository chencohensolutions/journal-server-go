package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/jpeg"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/media"
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
	binary.Write(&b, binary.BigEndian, uint32(0))
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

// add writes an original and a sidecar as an older import left it: source whatsapp, the app under "app".
func add(t *testing.T, root, name string, kind media.Kind, data []byte, extra string) string {
	t.Helper()
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	path := filepath.Join(root, "media", "2016", "2016-05", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	sidecar := `{"format":1,"sha256":"` + sha + `","facts":{"fileName":"` + name + `","kind":"` + string(kind) +
		`","mimeType":"image/jpeg","size":` + itoa(len(data)) +
		`,"takenAt":"2016-05-01T10:00:00Z","source":"whatsapp","app":"Bumble","uploadedAt":"2026-10-20T17:50:53Z","uploadedBy":"import"` + extra + `}}`
	if err := os.WriteFile(path+".json", []byte(sidecar), 0o644); err != nil {
		t.Fatal(err)
	}
	return sha
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func facts(t *testing.T, root, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "media", "2016", "2016-05", name) + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var sidecar struct {
		Facts map[string]any `json:"facts"`
	}
	if err := json.Unmarshal(data, &sidecar); err != nil {
		t.Fatal(err)
	}
	return sidecar.Facts
}

func snapshot(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Stat(path) // not d.Info: a Windows listing can carry a folder's time from before its last change
		if err != nil {
			return err
		}
		b.WriteString(path + " " + info.ModTime().String())
		if !d.IsDir() {
			data, _ := os.ReadFile(path)
			sum := sha256.Sum256(data)
			b.WriteString(" " + hex.EncodeToString(sum[:]))
		}
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// testTools are libvips loaded into the test (or nothing, reading only DNGs), and ffprobe when installed.
func testTools(t *testing.T) (tools media.CameraTools, withLib, withVideos bool) {
	vips, header, err := media.FindVips(filepath.Join("..", "..", "vips", "bin", "vips"))
	if err != nil {
		vips, header, err = media.FindVips("")
	}
	if err == nil {
		previews := media.NewPreviews(vips, header)
		if _, err := previews.LoadLibrary("", 2); err == nil {
			tools, withLib = previews.CameraTools(), true
		} else {
			t.Logf("libvips can't be loaded into the test (%v): only the DNG is read", err)
		}
	} else {
		t.Log("libvips not installed: only the DNG is read")
	}
	if _, ffprobe, err := media.FindFFmpeg(""); err == nil {
		tools, withVideos = tools.WithFFprobe(ffprobe), true
	}
	return tools, withLib, withVideos
}

func TestReadCamerasDryRunThenReal(t *testing.T) {
	root := t.TempDir()
	tools, withLib, withVideos := testTools(t)
	dngSha := add(t, root, "x.dng", media.KindPhoto, cameraTIFF("samsung", "SM-G930F"), "")
	add(t, root, "plain.jpg", media.KindPhoto, []byte("no tags in here"), "")
	add(t, root, "had.jpg", media.KindPhoto, []byte("had"), `,"camera":{"make":"Apple","model":"iPhone 15 Pro"}`)
	if withLib {
		add(t, root, "canon.jpg", media.KindPhoto, cameraJPEG(t, "Canon", "Canon EOS 80D"), "")
	}
	if withVideos {
		ffmpeg, _, _ := media.FindFFmpeg("")
		video := filepath.Join(t.TempDir(), "v.mp4")
		if out, err := exec.Command(ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "color=c=black:s=16x16:d=0.2", "-pix_fmt", "yuv420p",
			"-metadata", "com.android.manufacturer=samsung", "-metadata", "com.android.model=SM-G930F",
			"-movflags", "use_metadata_tags", video).CombinedOutput(); err != nil {
			t.Logf("ffmpeg can't make the test video: %v %s", err, out)
			withVideos = false
		} else {
			data, _ := os.ReadFile(video)
			add(t, root, "v.mp4", media.KindVideo, data, "")
		}
	}
	// A server had the folder open, so it has its .cache.
	if _, err := media.OpenStore(root, time.Now); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, root)

	store, err := media.OpenStoreReadOnly(root, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var progress, out bytes.Buffer
	r, err := read(context.Background(), store, tools, withVideos, true, 2, &progress)
	if err != nil {
		t.Fatal(err)
	}
	r.print(&out, true)
	t.Log(out.String())
	if after := snapshot(t, root); after != before {
		t.Fatalf("the dry run wrote:\n%s\n%s", before, after)
	}
	samsung := 1
	if withVideos {
		samsung = 2
	}
	for _, want := range []string{"samsung SM-G930F  (" + itoa(samsung) + " imported; 2016)", "iPhone 15 Pro  (1 imported; 2016)", "       1  photos"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report doesn't say %q:\n%s", want, out.String())
		}
	}
	if withLib && !strings.Contains(out.String(), "Canon EOS 80D") {
		t.Errorf("no Canon:\n%s", out.String())
	}
	if _, ok := facts(t, root, "x.dng")["camera"]; ok {
		t.Fatal("the dry run wrote a camera")
	}

	store, err = media.OpenStore(root, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if r, err = read(context.Background(), store, tools, withVideos, false, 2, &progress); err != nil {
		t.Fatal(err)
	}
	if wantWritten := samsung + map[bool]int{true: 1}[withLib]; r.written != wantWritten {
		t.Fatalf("%d written, want %d", r.written, wantWritten)
	}
	dng := facts(t, root, "x.dng")
	if c, _ := dng["camera"].(map[string]any); c["make"] != "samsung" || c["model"] != "SM-G930F" {
		t.Fatalf("x.dng's camera: %v", dng)
	}
	// Only the camera changed: the source and the app's key are as they were on disk, and no correction.
	if dng["source"] != "whatsapp" || dng["app"] != "Bumble" || dng["screenshotApp"] != nil || dng["correctedAt"] != nil {
		t.Fatalf("x.dng's other facts changed: %v", dng)
	}
	if got, _ := store.Get(dngSha); got.Sidecar.Facts.Camera == nil {
		t.Fatal("the store doesn't know the camera it wrote")
	}
	// Running again reads only what has no camera yet.
	if r, err = read(context.Background(), store, tools, withVideos, false, 2, &progress); err != nil || r.written != 0 || r.read != 1 {
		t.Fatalf("second run: %+v %v", r, err)
	}
}
