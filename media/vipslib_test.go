package media

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

var (
	testLibOnce sync.Once
	testLib     *vipsLib
	testLibErr  error
)

// loadTestLib is libvips loaded into the test, from the server's bundle (vips/bin) or the system's; tests that need
// it skip without.
func loadTestLib(t testing.TB) *vipsLib {
	t.Helper()
	vips, _, err := FindVips(filepath.Join("..", "vips", "bin", "vips"))
	if err != nil {
		if vips, _, err = FindVips(""); err != nil {
			t.Skip("libvips not installed")
		}
	}
	testLibOnce.Do(func() { testLib, testLibErr = loadVipsLib(vipsLibraries(vips, ""), 4) })
	if testLibErr != nil {
		t.Skipf("libvips can't be loaded into the process: %v", testLibErr)
	}
	return testLib
}

func TestVipsLibSmoke(t *testing.T) {
	l := loadTestLib(t)
	t.Log("libvips", l.Version)
	dir := t.TempDir()
	src := filepath.Join(dir, "in.jpg")
	os.WriteFile(src, orientedJPEG(t), 0o644)
	err := l.onThread(func() error {
		img, err := l.load(src)
		if err != nil {
			return err
		}
		defer img.Release()
		w, h := img.size()
		t.Log("header", w, h, "orientation", img.orientation())
		th, err := l.thumbnailFile(src, 100, 100, vipsSizeDown)
		if err != nil {
			return err
		}
		defer th.Release()
		w, h = th.size()
		t.Log("thumbnail", w, h)
		if err := l.save(th, filepath.Join(dir, "out.jxl"), "[Q=85,keep=icc]"); err != nil {
			return err
		}
		back, err := l.load(filepath.Join(dir, "out.jxl"))
		if err != nil {
			return err
		}
		defer back.Release()
		rgba, err := l.rgba(back)
		if err != nil {
			return err
		}
		t.Log("jxl back", rgba.Bounds())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// bothWays runs a preview test twice: with the vips programs, and with libvips in the server (skipped when it
// can't be loaded).
func bothWays(t *testing.T, test func(t *testing.T, inProcess bool)) {
	t.Run("programs", func(t *testing.T) { test(t, false) })
	t.Run("in-process", func(t *testing.T) { test(t, true) })
}

// withLib gives previews the test's libvips, when inProcess.
func withLib(t *testing.T, previews *Previews, inProcess bool) *Previews {
	t.Helper()
	if inProcess {
		previews.lib = loadTestLib(t)
	}
	return previews
}

func redTopBlueBottom(t *testing.T, name string, img image.Image) {
	t.Helper()
	b := img.Bounds()
	top, bottom := img.At(b.Dx()/2, b.Dy()/8), img.At(b.Dx()/2, b.Dy()*7/8)
	if !isRed(top) || !isBlue(bottom) {
		t.Errorf("%s: not upright: top %v, bottom %v", name, top, bottom)
	}
}

// Each picture is made from the original, upright, at exactly the size asked: contain never enlarges, squeeze
// stretches, and a region is that part of the photo.
func TestOriginalPicturesAreExactAndUpright(t *testing.T) {
	bothWays(t, func(t *testing.T, inProcess bool) {
		vips, header, err := FindVips("")
		if err != nil {
			t.Skip("libvips not installed")
		}
		previews := withLib(t, NewPreviews(vips, header), inProcess)
		f := newFixture(t)
		item := f.upload(orientedJPEG(t), "rotated.jpg") // 400×200, orientation 6: upright 200×400, red over blue
		o, err := previews.Open(context.Background(), f.store, item, 400)
		if err != nil {
			t.Fatal(err)
		}
		defer o.Close()
		if w, h := o.Size(); inProcess && (w != 200 || h != 400) {
			t.Errorf("decoded original is %d×%d, want 200×400 upright", w, h)
		}
		for _, c := range []struct {
			spec PreviewSpec
			w, h int
		}{
			{PreviewSpec{Width: 100, Height: 100, Fit: "contain"}, 50, 100},
			{PreviewSpec{Width: 64, Height: 64, Fit: SqueezeFit}, 64, 64},
			{PreviewSpec{Width: 1000, Height: 1000, Fit: "contain"}, 200, 400}, // never enlarged
		} {
			img, err := o.Picture(c.spec)
			if err != nil {
				t.Fatal(err)
			}
			if b := img.Bounds(); b.Dx() != c.w || b.Dy() != c.h {
				t.Errorf("%+v: %v, want %d×%d", c.spec, b, c.w, c.h)
			}
			redTopBlueBottom(t, fmt.Sprintf("%+v", c.spec), img)
		}
		bottom, err := o.Region(Rect{0, 0.5, 1, 0.5}, 20, 40)
		if err != nil {
			t.Fatal(err)
		}
		if b := bottom.Bounds(); b.Dx() != 20 || b.Dy() != 40 || !isBlue(bottom.At(10, 2)) || !isBlue(bottom.At(10, 37)) {
			t.Errorf("the bottom half: %v, %v … %v", b, bottom.At(10, 2), bottom.At(10, 37))
		}
		if _, err := o.Picture(PreviewSpec{Width: 10, Height: 10, Fit: "cover"}); err == nil {
			t.Error("a cover picture of an original was made")
		}
	})
}

// 16-bit and grey originals come out as 8-bit RGB.
func TestOriginalsOfOtherSampleFormats(t *testing.T) {
	l := loadTestLib(t)
	vips, header, _ := FindVips(filepath.Join("..", "vips", "bin", "vips"))
	previews := NewPreviews(vips, header)
	previews.lib = l
	f := newFixture(t)
	deep := image.NewRGBA64(image.Rect(0, 0, 40, 20))
	grey := image.NewGray(image.Rect(0, 0, 40, 20))
	for y := range 20 {
		for x := range 40 {
			deep.Set(x, y, color.RGBA64{0xFFFF, 0x8000, 0, 0xFFFF})
			grey.Set(x, y, color.Gray{200})
		}
	}
	for name, img := range map[string]image.Image{"deep.png": deep, "grey.png": grey} {
		var data bytes.Buffer
		png.Encode(&data, img)
		o, err := previews.Open(context.Background(), f.store, f.upload(data.Bytes(), name), 40)
		if err != nil || o.img == nil {
			t.Fatalf("%s: in the server: %v", name, err)
		}
		pic, err := o.Picture(PreviewSpec{Width: 40, Height: 40, Fit: "contain"})
		o.Close()
		if err != nil {
			t.Fatal(err)
		}
		r, g, b, _ := pic.At(20, 10).RGBA()
		want := map[string][3]uint32{"deep.png": {255, 128, 0}, "grey.png": {200, 200, 200}}[name]
		if got := [3]uint32{r >> 8, g >> 8, b >> 8}; abs(int(got[0])-int(want[0])) > 2 || abs(int(got[1])-int(want[1])) > 2 || abs(int(got[2])-int(want[2])) > 2 {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
}

func abs(n int) int { return max(n, -n) }

// An original over the limit is decoded already shrunk to twice the largest side asked for.
func TestHugeOriginalsAreDecodedShrunk(t *testing.T) {
	l := loadTestLib(t)
	vips, header, _ := FindVips(filepath.Join("..", "vips", "bin", "vips"))
	previews := NewPreviews(vips, header)
	previews.lib = l
	defer func(limit int) { shrinkAbove = limit }(shrinkAbove)
	shrinkAbove = 10_000
	f := newFixture(t)
	o, err := previews.Open(context.Background(), f.store, f.upload(orientedJPEG(t), "huge.jpg"), 25)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if w, h := o.Size(); w != 25 || h != 50 {
		t.Errorf("decoded at %d×%d, want 25×50 (twice 25 on the long side, upright)", w, h)
	}
}

// A file libvips in the server can't read goes to the programs, from then on.
func TestFilesTheLibraryFailsOnGoToThePrograms(t *testing.T) {
	l := loadTestLib(t)
	vips, header, _ := FindVips(filepath.Join("..", "vips", "bin", "vips"))
	previews := NewPreviews(vips, header)
	previews.lib = l
	f := newFixture(t)
	item := f.upload([]byte("not a picture at all"), "broken.jpg")
	o, err := previews.Open(context.Background(), f.store, item, 100)
	if err != nil {
		t.Fatal(err)
	}
	if o.img != nil || previews.inProcess(item.Sha256()) {
		t.Fatal("still in the server after failing")
	}
	if _, err := o.Picture(PreviewSpec{Width: 10, Height: 10, Fit: "contain"}); err == nil {
		t.Error("a picture of a broken file")
	}
}

// A DNG whose raw data is in JPEG XL tiles is joined in memory into the same picture as one stream gives.
func TestTiledJPEGXLDNGsAreJoined(t *testing.T) {
	l := loadTestLib(t)
	vips, header, _ := FindVips(filepath.Join("..", "vips", "bin", "vips"))
	previews := NewPreviews(vips, header)
	previews.lib = l
	dir := t.TempDir()
	// A gradient, so a tile in the wrong place shows.
	const w, h = 400, 200
	pixels := make([]byte, 0, w*h*6)
	for y := range h {
		for x := range w {
			for _, v := range []int{x * 160, y * 320, (x + y) * 100} {
				pixels = append(pixels, byte(v>>8), byte(v))
			}
		}
	}
	ppm := filepath.Join(dir, "linear.ppm")
	os.WriteFile(ppm, append([]byte(fmt.Sprintf("P6\n%d %d\n65535\n", w, h)), pixels...), 0o644)
	whole := filepath.Join(dir, "whole.jxl")
	if err := previews.run(context.Background(), "copy", ppm, whole+"[lossless]"); err != nil {
		t.Skipf("libvips can't write JPEG XL: %v", err)
	}
	var tiled bytes.Buffer
	var pieces []section
	for _, at := range [][2]int{{0, 0}, {200, 0}, {0, 100}, {200, 100}} {
		tile := filepath.Join(dir, "tile.jxl")
		if err := previews.run(context.Background(), "extract_area", ppm, tile+"[lossless]",
			fmt.Sprint(at[0]), fmt.Sprint(at[1]), "200", "100"); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(tile)
		pieces = append(pieces, section{int64(tiled.Len()), int64(len(data))})
		tiled.Write(data)
	}
	stream, _ := os.ReadFile(whole)
	dng := filepath.Join(dir, "whole.dng")
	os.WriteFile(dng, s25DNG(w, h, stream, 1, nil), 0o644)
	d, err := readLinearDNG(dng)
	if err != nil || d == nil {
		t.Fatalf("readLinearDNG: %v", err)
	}
	tiles := filepath.Join(dir, "tiles.raw")
	os.WriteFile(tiles, tiled.Bytes(), 0o644)
	dt := *d
	dt.pieces, dt.tileWidth, dt.tileHeight = pieces, 200, 100

	var one, four *image.RGBA
	err = l.onThread(func() error {
		for _, c := range []struct {
			d    *linearDNG
			file string
			out  **image.RGBA
		}{{d, dng, &one}, {&dt, tiles, &four}} {
			img, err := previews.developedImage(c.d, c.file)
			if err != nil {
				return err
			}
			*c.out, err = l.rgba(img)
			img.Release()
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if one.Bounds() != four.Bounds() || !bytes.Equal(one.Pix, four.Pix) {
		t.Errorf("tiles joined differ from the one stream: %v vs %v", four.Bounds(), one.Bounds())
	}
}
