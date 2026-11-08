package importcmd

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/journal/server-go/media"
)

// The same picture: telling a copy of a photo (resized, recompressed, its EXIF changed) from another
// shot that only looks alike. dHash finds the photos worth comparing; then both are shrunk to the same
// small grey square and compared pixel by pixel, after checking they have the same shape and, when the
// same camera took both, the same moment.

const (
	pictureSide = 64   // the grey square photos are compared at
	samePSNR    = 35.0 // and more (dB): the same picture
	unsurePSNR  = 28.0 // to samePSNR: for the owner to decide; less: another shot
	shapeSlack  = 0.01 // how far the width-to-height ratios may differ
	shotSlack   = 10 * time.Millisecond
	compared    = 8 // most look-alikes a photo is compared with, closest dHash first
)

// picture is what the same-picture check needs of one photo.
type picture struct {
	grey          []uint8 // pictureSide × pictureSide
	width, height int     // upright; 0 when unknown
	camera        string  // EXIF make and model
	shot          *time.Time
	exact         bool       // shot has its milliseconds (EXIF sub-seconds, or the file name's)
	named         *time.Time // the date and time in its file name (cameras and screenshots name files by it)
	namedExact    bool       // named has its milliseconds
	name          string     // its file name
}

// pictureCache keeps every photo's picture made this run, by sha256 (nil: couldn't be read).
type pictureCache struct {
	mu       sync.Mutex
	pictures map[string]*picture
}

// pictureOf returns the photo's picture, made once; f is its facts when already read.
func (im *importer) pictureOf(ctx context.Context, sha, path string, f *facts) *picture {
	im.pictures.mu.Lock()
	p, ok := im.pictures.pictures[sha]
	im.pictures.mu.Unlock()
	if ok {
		return p
	}
	if f == nil {
		read := im.readFacts(ctx, path)
		f = &read
	}
	p, err := im.makePicture(ctx, path, f)
	if ctx.Err() != nil {
		return nil // stopped: not remembered as unreadable
	}
	if err != nil {
		p = nil
	}
	im.pictures.mu.Lock()
	im.pictures.pictures[sha] = p
	im.pictures.mu.Unlock()
	return p
}

// makePicture shrinks the photo with libvips (upright, after its EXIF orientation) to a grey square.
func (im *importer) makePicture(ctx context.Context, path string, f *facts) (*picture, error) {
	out, err := os.CreateTemp(im.workDir, "*.ppm")
	if err != nil {
		return nil, err
	}
	out.Close()
	defer os.Remove(out.Name())
	side := strconv.Itoa(pictureSide)
	cmd := exec.CommandContext(ctx, im.vips, "thumbnail", path, out.Name(), side, "--height", side, "--size", "force")
	if msg, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("vips thumbnail: %v: %s", err, strings.TrimSpace(string(msg)))
	}
	data, err := os.ReadFile(out.Name())
	if err != nil {
		return nil, err
	}
	grey, err := readPNM(data, pictureSide, pictureSide)
	if err != nil {
		return nil, err
	}
	p := &picture{grey: make([]uint8, len(grey))}
	for i, v := range grey {
		p.grey[i] = uint8(v / 1000)
	}
	if f.width != nil && f.height != nil {
		p.width, p.height = *f.width, *f.height
	}
	p.camera = strings.TrimSpace(f.exif["exif-ifd0-Make"] + " " + f.exif["exif-ifd0-Model"])
	p.shot, p.exact = shotTime(f.exif, filepath.Base(path))
	p.named, p.namedExact = namedTime(filepath.Base(path))
	p.name = filepath.Base(path)
	return p, nil
}

// copyMark is what is added to a copy's name: "(1)", " (2)", "-נערכה" or "-edited" (Google Photos'
// edited copy), "~20230328-021353" (Samsung's gallery), "_saved".
var copyMark = regexp.MustCompile(`(?i)(\s*\(\d+\)|-נערכה|-edited|~[\d-]+|_saved)$`)

// shotName is a file name without its extension and the marks a copy's name gets, in lower case.
func shotName(name string) string {
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	for {
		trimmed := copyMark.ReplaceAllString(stem, "")
		if trimmed == stem {
			break
		}
		stem = trimmed
	}
	return strings.ToLower(strings.TrimSpace(stem))
}

// numberedApart reports whether two file names are a camera's numbers for two shots: alike but for their
// digits (DSC_000429 and DSC_000430; 20220528_084736_05 and …_03), as a copy keeps its name. WhatsApp's
// are left out: it numbers each file it saves, so a photo sent twice has two numbers.
func numberedApart(a, b string) bool {
	if media.IsWhatsAppName(a) || media.IsWhatsAppName(b) {
		return false
	}
	sa, sb := shotName(a), shotName(b)
	if sa == sb || len(sa) != len(sb) || !strings.ContainsAny(sa, "0123456789") {
		return false
	}
	for i := range len(sa) {
		da, db := sa[i] >= '0' && sa[i] <= '9', sb[i] >= '0' && sb[i] <= '9'
		if da != db || (!da && sa[i] != sb[i]) {
			return false
		}
	}
	return true
}

// namedTime is the date and time in a file name, with its milliseconds when it has them
// (20260120_081253_035.jpg); nil for a name with no time (IMG-20251104-WA0040.jpg: when it was sent).
func namedTime(name string) (*time.Time, bool) {
	m := dateTimeInName.FindStringSubmatch(name)
	if m == nil {
		return nil, false
	}
	t, err := time.Parse("2006 01 02 15 04 05", strings.Join(m[1:], " "))
	if err != nil || !plausible(t) {
		return nil, false
	}
	if ms := burstNumber.FindStringSubmatch(name); ms != nil && ms[1]+ms[2] == t.Format("20060102150405") {
		n, _ := strconv.Atoi(ms[3])
		t = t.Add(time.Duration(n) * time.Millisecond)
		return &t, true
	}
	return &t, false
}

// shotTime is when the photo was taken by its EXIF (as written, without its offset, which the same
// camera writes alike), with the milliseconds from EXIF's sub-seconds, or else from the file name
// (20260120_081253_035.jpg) when its second is the same. Nil without an EXIF date.
func shotTime(exif map[string]string, name string) (*time.Time, bool) {
	value := exif["exif-ifd2-DateTimeOriginal"]
	if value == "" || strings.HasPrefix(value, "0000") {
		return nil, false
	}
	t, err := time.Parse("2006:01:02 15:04:05", value)
	if err != nil || !plausible(t) {
		return nil, false
	}
	// Sub-seconds are the digits after the point: "035" is 35 ms, "35" is 350 ms.
	if sub := strings.TrimSpace(exif["exif-ifd2-SubSecTimeOriginal"]); sub != "" {
		if f, err := strconv.ParseFloat("0."+sub, 64); err == nil {
			t = t.Add(time.Duration(f * float64(time.Second)).Round(time.Millisecond))
			return &t, true
		}
	}
	if m := burstNumber.FindStringSubmatch(name); m != nil && m[1]+m[2] == t.Format("20060102150405") {
		ms, _ := strconv.Atoi(m[3])
		t = t.Add(time.Duration(ms) * time.Millisecond)
		return &t, true
	}
	return &t, false
}

// verdict is what the same-picture check found of two photos that look alike.
type verdict int

const (
	verdictDifferent verdict = iota // another shot: both are kept
	verdictUnsure                   // for the owner to decide
	verdictSame                     // the same picture
)

// comparePictures tells whether b is the same picture as a, with the pixels' score (PSNR in dB; 0 when
// not compared) and why, for the report.
func comparePictures(a, b *picture) (verdict, float64, string) {
	if a == nil || b == nil {
		return verdictUnsure, 0, "couldn't be compared"
	}
	if why := differentShots(a, b); why != "" {
		return verdictDifferent, 0, why
	}
	if a.width > 0 && a.height > 0 && b.width > 0 && b.height > 0 {
		ra, rb := float64(a.width)/float64(a.height), float64(b.width)/float64(b.height)
		if math.Abs(ra-rb) > shapeSlack*max(ra, rb) {
			return verdictDifferent, 0, fmt.Sprintf("another shape (%d×%d, %d×%d)", a.width, a.height, b.width, b.height)
		}
	}
	score := psnr(a.grey, b.grey)
	switch {
	case score >= samePSNR:
		return verdictSame, score, "the same pixels"
	case score >= unsurePSNR:
		return verdictUnsure, score, "nearly the same pixels"
	}
	return verdictDifferent, score, "other pixels"
}

// differentShots says why two photos are two shots, empty when they may be one: the same camera took them
// at different moments (by EXIF), or their file names give different times or shot numbers (a copy keeps
// its name; two screenshots of one app, or two shots from a tripod, look alike but aren't the same).
func differentShots(a, b *picture) string {
	if a.camera != "" && strings.EqualFold(a.camera, b.camera) && a.shot != nil && b.shot != nil {
		if gap := apart(*a.shot, *b.shot, a.exact && b.exact); gap != "" {
			return "taken " + gap + " apart by the same camera"
		}
	}
	if a.named != nil && b.named != nil {
		if gap := apart(*a.named, *b.named, a.namedExact && b.namedExact); gap != "" {
			return "named " + gap + " apart"
		}
	}
	if numberedApart(a.name, b.name) {
		return "numbered as two shots"
	}
	return ""
}

// apart is how far apart two moments are when they are more than 10 ms apart (exact: both have their
// milliseconds) or in different seconds; empty when they may be one.
func apart(a, b time.Time, exact bool) string {
	gap := a.Sub(b).Abs()
	if exact {
		if gap > shotSlack {
			return gap.String()
		}
		return ""
	}
	if !a.Truncate(time.Second).Equal(b.Truncate(time.Second)) {
		return gap.Truncate(time.Second).String()
	}
	return ""
}

// psnr is how close two grey pictures are, in dB: 99 when identical.
func psnr(a, b []uint8) float64 {
	var sum float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	mse := sum / float64(len(a))
	if mse == 0 {
		return 99
	}
	return 10 * math.Log10(255*255/mse)
}

// pixels is the picture's size in pixels, 0 when unknown.
func (p *picture) pixels() int {
	if p == nil {
		return 0
	}
	return p.width * p.height
}
