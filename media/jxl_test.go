package media

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"path/filepath"
	"testing"
)

// testJXL loads libjxl from the bundle (vips/bin) or the system, or skips the test.
func testJXL(t *testing.T) *JXL {
	t.Helper()
	vips, _, err := FindVips(filepath.Join("..", "vips", "bin", "vips"))
	if err != nil {
		vips, _, _ = FindVips("")
	}
	x, err := LoadJXL(JXLLibraries(vips, ""))
	if err != nil {
		t.Skipf("libjxl not installed: %v", err)
	}
	return x
}

// photoJPEG is a JPEG of a gradient, quality q.
func photoJPEG(t *testing.T, width, height, q int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, color.RGBA{uint8(x * 255 / width), uint8(y * 255 / height), uint8((x + y) % 256), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestJXLKeepsTheJPEGByteForByte(t *testing.T) {
	x := testJXL(t)
	for name, original := range map[string][]byte{
		"gradient": photoJPEG(t, 640, 480, 90),
		"exif":     withExif(photoJPEG(t, 64, 64, 90), "samsung", "SM-S931B"),
	} {
		jxl, err := x.Keep(original)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.HasPrefix(jxl, []byte{0, 0, 0, 0x0c, 'J', 'X', 'L', ' '}) {
			t.Errorf("%s: not a JPEG XL container: % x", name, jxl[:12])
		}
		again, err := x.ToJPEG(jxl)
		if err != nil || !bytes.Equal(again, original) {
			t.Errorf("%s: the JPEG made again differs (%d bytes, was %d): %v", name, len(again), len(original), err)
		}
		t.Logf("%s: %d bytes as JPEG, %d as JPEG XL (libjxl %s)", name, len(original), len(jxl), x.Version)
	}
}

// withExif puts an Exif block naming a camera right after the JPEG's start.
func withExif(data []byte, maker, model string) []byte {
	tiff := cameraTIFF(maker, model)
	app1 := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(app1[2:], uint16(2+6+len(tiff)))
	app1 = append(append(app1, "Exif\x00\x00"...), tiff...)
	return append(append(append([]byte{}, data[:2]...), app1...), data[2:]...)
}

func TestJXLRefusesWhatItCantKeep(t *testing.T) {
	x := testJXL(t)
	if _, err := x.Keep([]byte("not a jpeg at all")); err == nil {
		t.Error("kept something that isn't a JPEG")
	}
	// Go's grayscale JPEGs are one libjxl can't make again byte for byte: such a JPEG stays a JPEG.
	if _, err := x.Keep(cameraJPEG(t, "Canon", "Canon EOS 80D")); err == nil {
		t.Error("kept a JPEG libjxl refuses")
	}
	if _, err := x.ToJPEG([]byte("not a jpeg xl")); err == nil {
		t.Error("made a JPEG from something that isn't a JPEG XL")
	}
}
