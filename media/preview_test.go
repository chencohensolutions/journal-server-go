package media

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"net/url"
	"os"
	"testing"
)

func TestPlanPreview(t *testing.T) {
	for name, tc := range map[string]struct {
		spec          PreviewSpec
		width, height int
		want          plan
	}{
		"cover crops the sides": {
			PreviewSpec{Width: 100, Height: 100, Fit: "cover"}, 400, 200,
			plan{scaledW: 200, scaledH: 100, left: 50, top: 0, outW: 100, outH: 100},
		},
		"contain fits inside": {
			PreviewSpec{Width: 100, Height: 100, Fit: "contain"}, 400, 200,
			plan{scaledW: 100, scaledH: 50, outW: 100, outH: 50},
		},
		"never scaled up, box shape kept": {
			PreviewSpec{Width: 1000, Height: 500, Fit: "cover"}, 200, 400,
			plan{scaledW: 200, scaledH: 400, left: 0, top: 150, outW: 200, outH: 100},
		},
		"crop is cut from the full resolution": {
			PreviewSpec{Width: 100, Height: 100, Fit: "cover", Crop: &Rect{0.5, 0.5, 0.25, 0.25}}, 4000, 4000,
			plan{scaledW: 400, scaledH: 400, left: 200, top: 200, outW: 100, outH: 100},
		},
	} {
		if got := planPreview(tc.spec, tc.width, tc.height); got != tc.want {
			t.Errorf("%s: %+v, want %+v", name, got, tc.want)
		}
	}
}

func TestParsePreviewSpec(t *testing.T) {
	spec, err := ParsePreviewSpec(url.Values{"w": {"1080"}, "h": {"1440"}, "crop": {"0.1,0.2,0.50001,0.5"}})
	if err != nil || spec.Fit != "cover" || spec.Format != "webp" || spec.fileName() != "1080x1440-cover-0.1,0.2,0.5,0.5.webp" {
		t.Fatalf("spec = %+v %q %v", spec, spec.fileName(), err)
	}
	if whole, _ := ParsePreviewSpec(url.Values{"w": {"10"}, "h": {"10"}, "crop": {"0,0,1,1"}}); whole.Crop != nil {
		t.Fatalf("whole-picture crop kept: %+v", whole.Crop)
	}
	for _, bad := range []url.Values{
		{"w": {"0"}, "h": {"10"}},
		{"w": {"10"}, "h": {"5000"}},
		{"w": {"10"}, "h": {"10"}, "fit": {"stretch"}},
		{"w": {"10"}, "h": {"10"}, "crop": {"0.5,0,0.6,1"}},
		{"w": {"10"}, "h": {"10"}, "crop": {"NaN,0,1,1"}},
		{"w": {"10"}, "h": {"10"}, "fmt": {"png"}},
	} {
		if _, err := ParsePreviewSpec(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

// orientedJPEG is 400×200 stored, left half red and right half blue, tagged EXIF orientation 6:
// shown upright it is 200×400 with red on top.
func orientedJPEG(t *testing.T) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 400, 200))
	for x := 0; x < 400; x++ {
		for y := 0; y < 200; y++ {
			c := color.RGBA{220, 20, 20, 255}
			if x >= 200 {
				c = color.RGBA{20, 20, 220, 255}
			}
			img.Set(x, y, c)
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	exif := []byte{0xFF, 0xE1, 0x00, 34, 'E', 'x', 'i', 'f', 0, 0,
		'M', 'M', 0, 42, 0, 0, 0, 8, // TIFF header, first IFD at 8
		0, 1, // one entry
		0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, 6, 0, 0, // Orientation, SHORT, 1, value 6
		0, 0, 0, 0} // no next IFD
	data := encoded.Bytes()
	return append(append(append([]byte{}, data[:2]...), exif...), data[2:]...)
}

func decodeJPEG(t *testing.T, path string) image.Image {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func isRed(c color.Color) bool  { r, _, b, _ := c.RGBA(); return r > 2*b }
func isBlue(c color.Color) bool { r, _, b, _ := c.RGBA(); return b > 2*r }

func TestPreviewsAreUprightAndExactSize(t *testing.T) {
	vips, header, err := FindVips("")
	if err != nil {
		t.Skip("libvips not installed")
	}
	f := newFixture(t)
	item := f.upload(orientedJPEG(t), "rotated.jpg")
	previews := NewPreviews(vips, header)
	get := func(q url.Values) image.Image {
		t.Helper()
		spec, err := ParsePreviewSpec(q)
		if err != nil {
			t.Fatal(err)
		}
		path, err := previews.Get(context.Background(), f.store, item, spec)
		if err != nil {
			t.Fatal(err)
		}
		return decodeJPEG(t, path)
	}

	whole := get(url.Values{"w": {"100"}, "h": {"200"}, "fit": {"contain"}, "fmt": {"jpeg"}})
	if b := whole.Bounds(); b.Dx() != 100 || b.Dy() != 200 {
		t.Fatalf("contain size = %v", b)
	}
	if !isRed(whole.At(50, 20)) || !isBlue(whole.At(50, 180)) {
		t.Fatalf("not upright: top %v bottom %v", whole.At(50, 20), whole.At(50, 180))
	}

	bottom := get(url.Values{"w": {"50"}, "h": {"30"}, "crop": {"0,0.5,1,0.5"}, "fmt": {"jpeg"}})
	if b := bottom.Bounds(); b.Dx() != 50 || b.Dy() != 30 {
		t.Fatalf("crop size = %v", b)
	}
	if !isBlue(bottom.At(5, 5)) || !isBlue(bottom.At(45, 25)) {
		t.Fatalf("crop shows the wrong part")
	}

	huge := get(url.Values{"w": {"4000"}, "h": {"4000"}, "fit": {"contain"}, "fmt": {"jpeg"}})
	if b := huge.Bounds(); b.Dx() != 200 || b.Dy() != 400 {
		t.Fatalf("scaled up: %v", b)
	}

	webp, err := ParsePreviewSpec(url.Values{"w": {"64"}, "h": {"64"}})
	if err != nil {
		t.Fatal(err)
	}
	path, err := previews.Get(context.Background(), f.store, item, webp)
	if data, _ := os.ReadFile(path); err != nil || len(data) < 12 || string(data[8:12]) != "WEBP" {
		t.Fatalf("webp preview: %v", err)
	}
	if err := f.store.TrimPreviews(1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("trim kept previews over the limit")
	}
}
