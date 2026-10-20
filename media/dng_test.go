package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image/color"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The Galaxy S25's colour tags (from a real Expert RAW DNG).
var (
	s25ColorMatrix1   = []float64{0.85059, -0.20117, -0.13965, -0.45508, 1.4072, 0.023438, -0.10547, 0.30566, 0.40918}
	s25ColorMatrix2   = []float64{1.752, -0.97168, -0.24512, -0.33203, 1.4209, -0.15918, 0.0048828, 0.18945, 0.45801}
	s25ForwardMatrix1 = []float64{0.61133, 0.1377, 0.21484, 0.20312, 0.75293, 0.043945, 0.0039062, -0.39355, 1.2139}
	s25ForwardMatrix2 = []float64{0.46094, 0.17871, 0.3252, 0.067383, 0.75684, 0.17578, -0.11914, -0.80469, 1.749}
	s25Calibration    = []float64{0.96289, 0, 0, 0, 1, 0, 0, 0, 1.0088}
	s25Neutral        = []float64{0.57617, 1, 0.56055}
)

// tiffEntry is one tag for testDNG: SHORT, LONG, RATIONAL, SRATIONAL or UNDEFINED values.
type tiffEntry struct {
	tag     uint16
	typ     uint16
	numbers []float64
	raw     []byte
}

// testDNG writes a little-endian DNG with one IFD and the given data appended.
func testDNG(entries []tiffEntry, data []byte, dataTag uint16) []byte {
	le := binary.LittleEndian
	sort.Slice(entries, func(i, j int) bool { return entries[i].tag < entries[j].tag })
	ifdSize := 2 + len(entries)*12 + 4
	extra := 8 + ifdSize
	var values bytes.Buffer
	encode := func(e tiffEntry) []byte {
		var b bytes.Buffer
		if e.typ == 7 {
			return e.raw
		}
		for _, v := range e.numbers {
			switch e.typ {
			case 3:
				binary.Write(&b, le, uint16(v))
			case 4:
				binary.Write(&b, le, uint32(v))
			case 5:
				binary.Write(&b, le, uint32(math.Round(v*1e6)))
				binary.Write(&b, le, uint32(1e6))
			case 10:
				binary.Write(&b, le, int32(math.Round(v*1e6)))
				binary.Write(&b, le, int32(1e6))
			}
		}
		return b.Bytes()
	}
	count := func(e tiffEntry) int {
		if e.typ == 7 {
			return len(e.raw)
		}
		return len(e.numbers)
	}
	// Values that don't fit in an entry, then the image data.
	offsets := map[uint16]int{}
	for _, e := range entries {
		if b := encode(e); len(b) > 4 {
			offsets[e.tag] = extra + values.Len()
			values.Write(b)
		}
	}
	dataAt := extra + values.Len()

	var out bytes.Buffer
	out.WriteString("II*\x00")
	binary.Write(&out, le, uint32(8))
	binary.Write(&out, le, uint16(len(entries)))
	for _, e := range entries {
		binary.Write(&out, le, e.tag)
		binary.Write(&out, le, e.typ)
		binary.Write(&out, le, uint32(count(e)))
		b := encode(e)
		if e.tag == dataTag {
			b = make([]byte, 4)
			le.PutUint32(b, uint32(dataAt))
		}
		if off, ok := offsets[e.tag]; ok {
			binary.Write(&out, le, uint32(off))
		} else {
			out.Write(append(b, make([]byte, 4-len(b))...))
		}
	}
	binary.Write(&out, le, uint32(0))
	out.Write(values.Bytes())
	out.Write(data)
	return out.Bytes()
}

// s25DNG is a DNG laid out like the Galaxy S25's Expert RAW: linear RGB compressed with JPEG XL in
// IFD0, with the S25's colour tags, +3 EV, and the given gain table map (nil for none).
func s25DNG(width, height int, jxl []byte, orientation int, gainTable []byte) []byte {
	entries := []tiffEntry{
		{tag: tagNewSubfileType, typ: 4, numbers: []float64{0}},
		{tag: tagWidth, typ: 4, numbers: []float64{float64(width)}},
		{tag: tagHeight, typ: 4, numbers: []float64{float64(height)}},
		{tag: tagCompression, typ: 3, numbers: []float64{compressionJPEGXL}},
		{tag: tagPhotometric, typ: 3, numbers: []float64{photometricLinear}},
		{tag: tagStripOffsets, typ: 4, numbers: []float64{0}},
		{tag: tagOrientation, typ: 3, numbers: []float64{float64(orientation)}},
		{tag: tagSamplesPerPixel, typ: 3, numbers: []float64{3}},
		{tag: tagStripCounts, typ: 4, numbers: []float64{float64(len(jxl))}},
		{tag: tagWhiteLevel, typ: 4, numbers: []float64{65535, 65535, 65535}},
		{tag: tagColorMatrix1, typ: 10, numbers: s25ColorMatrix1},
		{tag: tagColorMatrix2, typ: 10, numbers: s25ColorMatrix2},
		{tag: tagCameraCalibration1, typ: 10, numbers: s25Calibration},
		{tag: tagCameraCalibration2, typ: 10, numbers: s25Calibration},
		{tag: tagAsShotNeutral, typ: 5, numbers: s25Neutral},
		{tag: tagBaselineExposure, typ: 10, numbers: []float64{3}},
		{tag: tagIlluminant1, typ: 3, numbers: []float64{21}},
		{tag: tagIlluminant2, typ: 3, numbers: []float64{17}},
		{tag: tagForwardMatrix1, typ: 10, numbers: s25ForwardMatrix1},
		{tag: tagForwardMatrix2, typ: 10, numbers: s25ForwardMatrix2},
	}
	if gainTable != nil {
		entries = append(entries, tiffEntry{tag: tagGainTableMap, typ: 7, raw: gainTable})
	}
	return testDNG(entries, jxl, tagStripOffsets)
}

// flatGainTable is a 1×1 ProfileGainTableMap with the given gain for every brightness.
func flatGainTable(gain float32) []byte {
	le := binary.LittleEndian
	var b bytes.Buffer
	binary.Write(&b, le, uint32(1))
	binary.Write(&b, le, uint32(1))
	for _, v := range []float64{1, 1, 0.5, 0.5} {
		binary.Write(&b, le, v)
	}
	binary.Write(&b, le, uint32(2))
	for _, w := range []float32{0.33, 0.33, 0.34, 0, 0} {
		binary.Write(&b, le, w)
	}
	binary.Write(&b, le, gain)
	binary.Write(&b, le, gain)
	return b.Bytes()
}

func TestReadLinearDNG(t *testing.T) {
	file := writeTemp(t, s25DNG(400, 200, []byte("jpeg xl data"), 6, flatGainTable(2)))
	d, err := readLinearDNG(file)
	if err != nil || d == nil {
		t.Fatalf("readLinearDNG: %v %v", d, err)
	}
	if d.width != 400 || d.height != 200 || d.orientation != 6 || len(d.pieces) != 1 || d.pieces[0].length != 12 {
		t.Errorf("read %+v", d)
	}
	if d.white != [3]float64{65535, 65535, 65535} || d.baselineExposure != 3 || d.illuminant != [2]int{21, 17} {
		t.Errorf("levels %v exposure %v illuminants %v", d.white, d.baselineExposure, d.illuminant)
	}
	if d.gainTable == nil || d.gainTable.gain(0.3, 0.7, 0.5) != 2 {
		t.Errorf("gain table %+v", d.gainTable)
	}

	// A DNG with other raw data (here uncompressed) is left to libvips.
	other := s25DNG(400, 200, []byte("x"), 1, nil)
	other = bytes.Replace(other, []byte{0x03, 0x01, 0x03, 0x00, 0x01, 0x00, 0x00, 0x00, 0x42, 0xCD}, []byte{0x03, 0x01, 0x03, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x00}, 1)
	if d, err := readLinearDNG(writeTemp(t, other)); err != nil || d != nil {
		t.Errorf("uncompressed DNG: %v %v", d, err)
	}
	if _, err := readLinearDNG(writeTemp(t, []byte("not a tiff at all"))); err == nil {
		t.Error("accepted a file that isn't a DNG")
	}
}

func writeTemp(t *testing.T, data []byte) string {
	file := filepath.Join(t.TempDir(), "file.dng")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestCameraToSRGBMakesTheShotsNeutralGrey(t *testing.T) {
	d, err := readLinearDNG(writeTemp(t, s25DNG(4, 4, []byte("x"), 1, nil)))
	if err != nil {
		t.Fatal(err)
	}
	m, err := d.cameraToSRGB()
	if err != nil {
		t.Fatal(err)
	}
	got := m.apply([3]float64(s25Neutral))
	for c := range got {
		if math.Abs(got[c]-1) > 0.002 {
			t.Fatalf("neutral → %v, want white", got)
		}
	}

	// Without ForwardMatrix, the ColorMatrix path also keeps the neutral grey.
	d.forwardMatrix = [2]*matrix3{}
	if m, err = d.cameraToSRGB(); err != nil {
		t.Fatal(err)
	}
	got = m.apply([3]float64(s25Neutral))
	if math.Abs(got[0]-got[1]) > 0.01 || math.Abs(got[2]-got[1]) > 0.01 || math.Abs(got[1]-1) > 0.05 {
		t.Fatalf("ColorMatrix only: neutral → %v", got)
	}
}

func TestGainTableInterpolates(t *testing.T) {
	g := &gainTableMap{pointsV: 1, pointsH: 2, pointsN: 2, spacingV: 1, spacingH: 1, originV: 0, originH: 0,
		gains: []float32{1, 3, 2, 4}} // h=0: 1 (dark) → 3 (bright); h=1: 2 → 4
	for _, c := range []struct{ h, b, want float64 }{{0, 0, 1}, {0, 1, 3}, {1, 0, 2}, {0.5, 0.5, 2.5}, {-1, 2, 3}} {
		if got := g.gain(0, c.h, c.b); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("gain(h %v, b %v) = %v, want %v", c.h, c.b, got, c.want)
		}
	}
}

// linearPPM is a 16-bit PPM width×height: the left half camera value dark, the right half bright.
func linearPPM(width, height int, dark, bright [3]uint16) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "P6\n%d %d\n65535\n", width, height)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			v := dark
			if x >= width/2 {
				v = bright
			}
			for _, s := range v {
				b.WriteByte(byte(s >> 8))
				b.WriteByte(byte(s))
			}
		}
	}
	return b.Bytes()
}

func TestPreviewsOfALinearDNGAreDevelopedFromItsRawData(t *testing.T) {
	vips, header, err := FindVips("")
	if err != nil {
		t.Skip("libvips not installed")
	}
	previews := NewPreviews(vips, header)
	dir := t.TempDir()
	// Camera values of the shot's neutral at 10% and 60% of white, after the DNG's +3 EV.
	camera := func(level float64) (v [3]uint16) {
		for c := range v {
			v[c] = uint16(math.Round(s25Neutral[c] * level / 8 * 65535))
		}
		return v
	}
	ppm := filepath.Join(dir, "linear.ppm")
	if err := os.WriteFile(ppm, linearPPM(400, 200, camera(0.1), camera(0.6)), 0o644); err != nil {
		t.Fatal(err)
	}
	jxl := filepath.Join(dir, "linear.jxl")
	if err := previews.run(context.Background(), "copy", ppm, jxl+"[lossless]"); err != nil {
		t.Skipf("libvips can't write JPEG XL: %v", err)
	}
	stream, err := os.ReadFile(jxl)
	if err != nil {
		t.Fatal(err)
	}

	f := newFixture(t)
	spec, err := ParsePreviewSpec(url.Values{"w": {"100"}, "h": {"200"}, "fit": {"contain"}, "fmt": {"jpeg"}})
	if err != nil {
		t.Fatal(err)
	}
	luma := func(c color.Color) float64 {
		r, g, b, _ := c.RGBA()
		return (float64(r) + float64(g) + float64(b)) / 3 / 257
	}
	grey := func(c color.Color) bool {
		r, g, b, _ := c.RGBA()
		return max(r, g, b)-min(r, g, b) < 6*257
	}
	var plain float64
	for _, c := range []struct {
		name string
		gain []byte
	}{{"plain.dng", nil}, {"lifted.dng", flatGainTable(2)}} {
		dng := s25DNG(400, 200, stream, 6, c.gain)
		req := photoRequest(dng, c.name)
		req.MimeType = "image/x-adobe-dng"
		f.start(req)
		result, err := f.chunk(dng, 0, int64(len(dng)))
		if err != nil || result.Item == nil {
			t.Fatalf("upload %s: %v", c.name, err)
		}
		path, err := previews.Get(context.Background(), f.store, *result.Item, spec)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		img := decodeJPEG(t, path)
		if b := img.Bounds(); b.Dx() != 100 || b.Dy() != 200 {
			t.Fatalf("%s: size = %v (not upright?)", c.name, b)
		}
		top, bottom := img.At(50, 20), img.At(50, 180)
		if !grey(top) || !grey(bottom) {
			t.Fatalf("%s: the shot's neutral isn't grey: %v %v", c.name, top, bottom)
		}
		if luma(top) >= luma(bottom) {
			t.Fatalf("%s: dark half (orientation 6 puts it on top) %v, bright %v", c.name, top, bottom)
		}
		if c.gain == nil {
			// 10% linear is about 89 in sRGB.
			if l := luma(top); l < 80 || l > 98 {
				t.Errorf("10%% grey developed to %.0f", l)
			}
			plain = luma(top)
		} else if luma(top) <= plain+20 {
			t.Errorf("the gain table didn't lift the shadows: %.0f vs %.0f", luma(top), plain)
		}
		if _, err := os.Stat(filepath.Join(f.store.previewDir(result.Item.Sha256()), developedName)); err != nil {
			t.Errorf("%s: developed image not kept: %v", c.name, err)
		}
	}
}
