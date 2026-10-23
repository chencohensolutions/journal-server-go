package ocr

import (
	"image"
	"image/color"
	"image/draw"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"github.com/journal/server-go/onnx"
)

// A font with Hebrew, from the system; tests that need one skip without.
func hebrewFont(t *testing.T) font.Face {
	t.Helper()
	candidates := []string{`C:\Windows\Fonts\arial.ttf`, "/System/Library/Fonts/Supplemental/Arial.ttf",
		"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"}
	for _, path := range candidates {
		if data, err := os.ReadFile(path); err == nil {
			if f, err := opentype.Parse(data); err == nil {
				face, _ := opentype.NewFace(f, &opentype.FaceOptions{Size: 28, DPI: 72})
				return face
			}
		}
	}
	t.Skipf("no font with Hebrew on %s", runtime.GOOS)
	return nil
}

// visual is Hebrew as a screen draws it, right to left: the runes reversed (enough for words without numbers).
func visual(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

// Rendered Hebrew and English come back as written, Hebrew in logical order.
func TestHebrewAndEnglishAreRead(t *testing.T) {
	folder := filepath.Join("..", "models", "paddleocr-hebrew")
	library := filepath.Join("..", "onnxruntime", onnx.RuntimeLibrary())
	for _, need := range []string{filepath.Join(folder, "det.onnx"), filepath.Join(folder, "rec.onnx"), library} {
		if _, err := os.Stat(need); err != nil {
			t.Skipf("%s is missing (go run ./scripts/bundle-onnx)", need)
		}
	}
	face := hebrewFont(t)
	rt, err := onnx.LoadRuntime(library)
	if err != nil {
		t.Fatal(err)
	}
	m, err := LoadModel(rt, folder, 4)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		text string
		rtl  bool
	}{{"שלום עולם", true}, {"Invoice total 349.90", false}, {"משלוח חינם", true}}
	img := image.NewRGBA(image.Rect(0, 0, 700, 60*len(want)+40))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	for i, w := range want {
		text := w.text
		if w.rtl {
			text = visual(text)
		}
		d := font.Drawer{Dst: img, Src: image.NewUniform(color.Black), Face: face, Dot: fixed.P(40, 60+60*i)}
		d.DrawString(text)
	}
	lines, err := m.Read(img, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range lines {
		got = append(got, l.Text)
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			found = found || strings.Contains(g, w.text)
		}
		if !found {
			t.Errorf("%q not read; got %q", w.text, got)
		}
	}
}
