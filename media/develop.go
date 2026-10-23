package media

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"unsafe"
)

// Developing a DNG whose raw data is demosaiced and compressed with JPEG XL (Samsung's Expert RAW and
// Pro mode, DNG 1.7), which LibRaw can't unpack: libvips decodes the JPEG XL (its jxlload), and this
// file applies the DNG's colour processing (DNG specification 1.7, chapter 6): black and white levels,
// white balance and the camera's matrices to linear sRGB, BaselineExposure, the ProfileGainTableMap
// (local tone mapping), a soft roll-off of highlights and the sRGB curve. The result is kept as
// developed.jpg next to the photo's previews, which are then made from it like from any photo.

const developedName = "developed.jpg"

// developed returns the developed image of a linear DNG. With the store's CachePreviews it is kept
// beside the photo's previews (and found there next time); without, it is made in into, the request's
// temporary folder.
func (p *Previews) developed(ctx context.Context, store *Store, item Item, d *linearDNG, original, into string) (string, error) {
	path := filepath.Join(into, developedName)
	if store.CachePreviews {
		path = filepath.Join(store.previewDir(item.Sha256()), developedName)
		unlock := p.locks.Lock(path)
		defer unlock()
	}
	if info, err := os.Stat(path); err == nil {
		touch(path, info)
		return path, nil
	}
	work, err := os.MkdirTemp(filepath.Join(store.root, ".cache"), "develop-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)

	// The JPEG XL stream (or one per tile) as a 16-bit PPM, which this file reads.
	f, err := os.Open(original)
	if err != nil {
		return "", err
	}
	var streams []string
	for i, piece := range d.pieces {
		stream := filepath.Join(work, fmt.Sprintf("piece%d.jxl", i))
		out, err := os.Create(stream)
		if err != nil {
			f.Close()
			return "", err
		}
		_, err = io.Copy(out, io.NewSectionReader(f, piece.offset, piece.length))
		out.Close()
		if err != nil {
			f.Close()
			return "", err
		}
		streams = append(streams, stream)
	}
	f.Close()
	linear := filepath.Join(work, "linear.ppm")
	if len(streams) == 1 {
		err = p.run(ctx, "copy", streams[0], linear)
	} else {
		across := (d.width + d.tileWidth - 1) / d.tileWidth
		joined := filepath.Join(work, "joined.v")
		if err = p.run(ctx, "arrayjoin", joinArgs(streams), joined, "--across", strconv.Itoa(across)); err == nil {
			err = p.run(ctx, "extract_area", joined, linear, "0", "0", strconv.Itoa(d.width), strconv.Itoa(d.height))
		}
	}
	if err != nil {
		return "", fmt.Errorf("decoding the JPEG XL raw data (libvips needs JPEG XL support): %w", err)
	}

	srgb := filepath.Join(work, "developed.ppm")
	if err := developPPM(linear, srgb, d); err != nil {
		return "", err
	}
	encoded := filepath.Join(work, "encoded.jpg")
	if err := p.run(ctx, "copy", srgb, encoded+"[Q=95]"); err != nil {
		return "", err
	}
	data, err := os.ReadFile(encoded)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := writeFileAtomic(path, withOrientation(data, d.orientation)); err != nil {
		return "", err
	}
	return path, nil
}

// joinArgs is the space-separated list vips takes for an array of images.
func joinArgs(files []string) string {
	var b bytes.Buffer
	for i, f := range files {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(f)
	}
	return b.String()
}

// developPPM reads the decoded raw data (16-bit PPM) and writes the developed image (8-bit sRGB PPM),
// cut to the DNG's default crop.
func developPPM(in, out string, d *linearDNG) error {
	width, height, pixels, err := readPPM16(in)
	if err != nil {
		return err
	}
	developed, err := develop(width, height, pixels, d)
	if err != nil {
		return err
	}
	file, err := os.Create(out)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(file)
	fmt.Fprintf(bw, "P6\n%d %d\n255\n", d.cropW, d.cropH)
	bw.Write(developed)
	if err := bw.Flush(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// develop is the decoded raw data (width×height, 16-bit RGB) developed: 8-bit sRGB, cut to the DNG's default crop
// (cropW×cropH).
func develop(width, height int, pixels []uint16, d *linearDNG) ([]byte, error) {
	if width < d.cropX+d.cropW || height < d.cropY+d.cropH {
		return nil, fmt.Errorf("decoded raw data is %dx%d, the DNG says %dx%d", width, height, d.width, d.height)
	}
	transform, err := d.cameraToSRGB()
	if err != nil {
		return nil, err
	}
	exposure := math.Exp2(d.baselineExposure)
	var scale, black [3]float64
	for c := range scale {
		scale[c] = exposure / math.Max(1, d.white[c]-d.black[c])
		black[c] = d.black[c]
	}
	curve := toneCurve()

	w, h := d.cropW, d.cropH
	developed := make([]byte, w*h*3)
	rows := make(chan int, h)
	for y := range h {
		rows <- y
	}
	close(rows)
	var wg sync.WaitGroup
	for range runtime.NumCPU() {
		wg.Go(func() {
			for y := range rows {
				src := pixels[((y+d.cropY)*width+d.cropX)*3:]
				dst := developed[y*w*3:]
				v := (float64(y) + 0.5) / float64(h)
				for x := range w {
					var camera [3]float64
					for c := range camera {
						camera[c] = (float64(src[x*3+c]) - black[c]) * scale[c]
					}
					rgb := transform.apply(camera)
					if g := d.gainTable; g != nil {
						gain := g.gain(v, (float64(x)+0.5)/float64(w), g.brightness(rgb[0], rgb[1], rgb[2]))
						for c := range rgb {
							rgb[c] *= gain
						}
					}
					for c := range rgb {
						dst[x*3+c] = curve.lookup(rgb[c])
					}
				}
			}
		})
	}
	wg.Wait()
	return developed, nil
}

// developedImage is a linear DNG developed by libvips in the server, in memory, with the DNG's orientation (which
// thumbnail turns it by): each JPEG XL piece decoded from the file's bytes, the tiles joined and cut to the raw
// size, then developed as developPPM does. No temporary files, no programs. Runs on a libvips thread.
func (p *Previews) developedImage(d *linearDNG, original string) (*vipsImage, error) {
	f, err := os.Open(original)
	if err != nil {
		return nil, err
	}
	var tiles []*vipsImage
	defer func() {
		for _, t := range tiles {
			t.Release()
		}
	}()
	for _, piece := range d.pieces {
		data := make([]byte, piece.length)
		if _, err := f.ReadAt(data, piece.offset); err != nil {
			f.Close()
			return nil, fmt.Errorf("reading the raw data: %w", err)
		}
		tile, err := p.lib.loadJXL(data)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("decoding the JPEG XL raw data: %w", err)
		}
		tiles = append(tiles, tile)
	}
	f.Close()
	if len(tiles) == 0 {
		return nil, errors.New("the DNG has no raw data")
	}
	raw := tiles[0]
	if len(tiles) > 1 {
		joined, err := p.lib.arrayJoin(tiles, (d.width+d.tileWidth-1)/d.tileWidth)
		if err != nil {
			return nil, err
		}
		defer joined.Release()
		if raw, err = p.lib.extractArea(joined, 0, 0, d.width, d.height); err != nil {
			return nil, err
		}
		defer raw.Release()
	}
	width, height, pixels, err := p.lib.rgb16(raw)
	if err != nil {
		return nil, err
	}
	developed, err := develop(width, height, pixels, d)
	if err != nil {
		return nil, err
	}
	img, err := p.lib.fromMemory(unsafe.Pointer(&developed[0]), uintptr(len(developed)), d.cropW, d.cropH, 3, vipsFormatUchar)
	runtime.KeepAlive(developed)
	if err != nil {
		return nil, err
	}
	p.lib.setOrientation(img, d.orientation)
	return img, nil
}

// readPPM16 reads a binary PPM with 16-bit samples, as libvips writes it (comments allowed).
func readPPM16(file string) (width, height int, pixels []uint16, err error) {
	f, err := os.Open(file)
	if err != nil {
		return 0, 0, nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var fields []int
	magic := ""
	for len(fields) < 3 {
		word, err := ppmWord(r)
		if err != nil {
			return 0, 0, nil, err
		}
		if magic == "" {
			magic = word
			continue
		}
		n, err := strconv.Atoi(word)
		if err != nil {
			return 0, 0, nil, fmt.Errorf("PPM header: %q", word)
		}
		fields = append(fields, n)
	}
	width, height = fields[0], fields[1]
	if magic != "P6" || fields[2] != 65535 || width <= 0 || height <= 0 {
		return 0, 0, nil, fmt.Errorf("not a 16-bit PPM (%s, max %d)", magic, fields[2])
	}
	raw := make([]byte, width*height*6)
	if _, err := io.ReadFull(r, raw); err != nil {
		return 0, 0, nil, err
	}
	pixels = make([]uint16, width*height*3)
	for i := range pixels {
		pixels[i] = uint16(raw[i*2])<<8 | uint16(raw[i*2+1])
	}
	return width, height, pixels, nil
}

// ppmWord reads one header word, skipping white space and comments; it consumes the single white
// space character after it.
func ppmWord(r *bufio.Reader) (string, error) {
	var word []byte
	for {
		c, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		switch {
		case c == '#' && len(word) == 0:
			if _, err := r.ReadString('\n'); err != nil {
				return "", err
			}
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if len(word) > 0 {
				return string(word), nil
			}
		default:
			word = append(word, c)
		}
	}
}

// curveTable maps linear light (1 is white, brighter is rolled off) to 8-bit sRGB.
type curveTable struct {
	values []byte
	step   float64
}

const (
	// Where highlights start to roll off softly toward white instead of clipping.
	shoulder = 0.8
	// The table covers linear values up to this; brighter ones are white.
	curveMax = 8.0
)

func toneCurve() curveTable {
	t := curveTable{values: make([]byte, 1<<16), step: curveMax / (1<<16 - 1)}
	for i := range t.values {
		x := float64(i) * t.step
		if x > shoulder {
			x = shoulder + (1-shoulder)*(1-math.Exp(-(x-shoulder)/(1-shoulder)))
		}
		if x <= 0.0031308 {
			x *= 12.92
		} else {
			x = 1.055*math.Pow(x, 1/2.4) - 0.055
		}
		t.values[i] = byte(math.Round(math.Max(0, math.Min(1, x)) * 255))
	}
	return t
}

func (t curveTable) lookup(x float64) byte {
	if x <= 0 {
		return t.values[0]
	}
	i := int(x / t.step)
	if i >= len(t.values) {
		return t.values[len(t.values)-1]
	}
	return t.values[i]
}

// matrix3 is a 3×3 matrix, row by row.
type matrix3 [9]float64

func (a matrix3) mul(b matrix3) (c matrix3) {
	for i := range 3 {
		for j := range 3 {
			for k := range 3 {
				c[i*3+j] += a[i*3+k] * b[k*3+j]
			}
		}
	}
	return c
}

func (a matrix3) apply(v [3]float64) (r [3]float64) {
	for i := range 3 {
		r[i] = a[i*3]*v[0] + a[i*3+1]*v[1] + a[i*3+2]*v[2]
	}
	return r
}

func (a matrix3) inverse() (matrix3, error) {
	det := a[0]*(a[4]*a[8]-a[5]*a[7]) - a[1]*(a[3]*a[8]-a[5]*a[6]) + a[2]*(a[3]*a[7]-a[4]*a[6])
	if math.Abs(det) < 1e-12 {
		return matrix3{}, errors.New("colour matrix can't be inverted")
	}
	return matrix3{
		(a[4]*a[8] - a[5]*a[7]) / det, (a[2]*a[7] - a[1]*a[8]) / det, (a[1]*a[5] - a[2]*a[4]) / det,
		(a[5]*a[6] - a[3]*a[8]) / det, (a[0]*a[8] - a[2]*a[6]) / det, (a[2]*a[3] - a[0]*a[5]) / det,
		(a[3]*a[7] - a[4]*a[6]) / det, (a[1]*a[6] - a[0]*a[7]) / det, (a[0]*a[4] - a[1]*a[3]) / det,
	}, nil
}

func diagonal(v [3]float64) matrix3 { return matrix3{v[0], 0, 0, 0, v[1], 0, 0, 0, v[2]} }

func identity() matrix3 { return diagonal([3]float64{1, 1, 1}) }

// mix interpolates two matrices: weight 1 is a, 0 is b. A missing b is a.
func mix(a, b *matrix3, weight float64) matrix3 {
	if b == nil {
		return *a
	}
	var c matrix3
	for i := range c {
		c[i] = weight*a[i] + (1-weight)*b[i]
	}
	return c
}

var (
	// CIE XYZ (D50) to linear sRGB (D65), with Bradford adaptation.
	xyzD50ToSRGB = matrix3{3.1338561, -1.6168667, -0.4906146, -0.9787684, 1.9161415, 0.0334540, 0.0719453, -0.2289914, 1.4052427}
	bradford     = matrix3{0.8951, 0.2664, -0.1614, -0.7502, 1.7135, 0.0367, 0.0389, -0.0685, 1.0296}
	d50XY        = [2]float64{0.34567, 0.35850}
)

// illuminantTemperature is the colour temperature of an EXIF LightSource value (0 when unknown).
func illuminantTemperature(code int) float64 {
	return map[int]float64{
		1: 5500, 2: 4150, 3: 2850, 4: 5500, 9: 5500, 10: 6500, 11: 7500, 12: 6430, 13: 5000, 14: 4150,
		15: 3450, 16: 2940, 17: 2856, 18: 4874, 19: 6774, 20: 5503, 21: 6504, 22: 7504, 23: 5003, 24: 3200,
	}[code]
}

// temperature is McCamy's approximation of the correlated colour temperature of xy.
func temperature(xy [2]float64) float64 {
	n := (xy[0] - 0.3320) / (0.1858 - xy[1])
	return 449*n*n*n + 3525*n*n + 6823.3*n + 5520.33
}

func xyToXYZ(xy [2]float64) [3]float64 {
	return [3]float64{xy[0] / xy[1], 1, (1 - xy[0] - xy[1]) / xy[1]}
}

func xyzToXY(v [3]float64) [2]float64 {
	s := v[0] + v[1] + v[2]
	if s <= 0 {
		return d50XY
	}
	return [2]float64{v[0] / s, v[1] / s}
}

// cameraToSRGB is the matrix from the raw data, black-subtracted and scaled to 0–1, to linear sRGB,
// white-balanced for the shot (DNG specification, "Mapping Camera Color Space to CIE XYZ Space").
func (d *linearDNG) cameraToSRGB() (matrix3, error) {
	t1, t2 := illuminantTemperature(d.illuminant[0]), illuminantTemperature(d.illuminant[1])
	weight := func(xy [2]float64) float64 {
		if d.colorMatrix[1] == nil || t1 == 0 || t2 == 0 || t1 == t2 {
			return 1
		}
		t := temperature(xy)
		return math.Max(0, math.Min(1, (1/t-1/t2)/(1/t1-1/t2)))
	}
	ab := diagonal(d.analogBalance)
	calibration := func(i int) *matrix3 {
		if d.cameraCalibration[i] == nil {
			m := identity()
			return &m
		}
		return d.cameraCalibration[i]
	}
	// XYZ → camera at the white with weight g: AB × CC × CM.
	xyzToCamera := func(g float64) matrix3 {
		cc := mix(calibration(0), calibration(1), g)
		return ab.mul(cc).mul(mix(d.colorMatrix[0], d.colorMatrix[1], g))
	}

	// The white of the shot, found from the camera's neutral by iterating with the interpolated matrix.
	white := d50XY
	var g float64
	if d.asShotWhiteXY != nil {
		white = *d.asShotWhiteXY
		g = weight(white)
	} else {
		neutral := [3]float64{1, 1, 1}
		if d.asShotNeutral != nil {
			neutral = *d.asShotNeutral
		}
		for range 30 {
			g = weight(white)
			inverse, err := xyzToCamera(g).inverse()
			if err != nil {
				return matrix3{}, err
			}
			next := xyzToXY(inverse.apply(neutral))
			if math.Abs(next[0]-white[0]) < 1e-7 && math.Abs(next[1]-white[1]) < 1e-7 {
				white = next
				break
			}
			white = next
		}
	}

	cameraNeutral := xyzToCamera(g).apply(xyToXYZ(white))
	if d.asShotNeutral != nil {
		cameraNeutral = *d.asShotNeutral
	}
	abcc := ab.mul(mix(calibration(0), calibration(1), g))
	abccInverse, err := abcc.inverse()
	if err != nil {
		return matrix3{}, err
	}

	var cameraToXYZ matrix3
	if d.forwardMatrix[0] != nil {
		// ForwardMatrix: white-balanced camera → XYZ D50, white mapping to D50.
		reference := abccInverse.apply(cameraNeutral)
		balance := diagonal([3]float64{1 / reference[0], 1 / reference[1], 1 / reference[2]})
		cameraToXYZ = mix(d.forwardMatrix[0], d.forwardMatrix[1], g).mul(balance).mul(abccInverse)
	} else {
		// ColorMatrix only: camera → XYZ at the shot's white, then adapted to D50 (Bradford).
		inverse, err := xyzToCamera(g).inverse()
		if err != nil {
			return matrix3{}, err
		}
		bradfordInverse, _ := bradford.inverse()
		from, to := bradford.apply(xyToXYZ(white)), bradford.apply(xyToXYZ(d50XY))
		adapt := bradfordInverse.mul(diagonal([3]float64{to[0] / from[0], to[1] / from[1], to[2] / from[2]})).mul(bradford)
		cameraToXYZ = adapt.mul(inverse)
		// Scaled so the camera's neutral is white (Y = 1).
		y := cameraToXYZ.apply(cameraNeutral)[1]
		for i := range cameraToXYZ {
			cameraToXYZ[i] /= y
		}
	}
	return xyzD50ToSRGB.mul(cameraToXYZ), nil
}

// withOrientation returns jpeg with an EXIF block saying orientation, so libvips turns it upright
// like the DNG it came from. Orientation 1 (or none) leaves it as it is.
func withOrientation(jpeg []byte, orientation int) []byte {
	if orientation <= 1 || orientation > 8 || len(jpeg) < 2 {
		return jpeg
	}
	exif := []byte{0xFF, 0xE1, 0x00, 34, 'E', 'x', 'i', 'f', 0, 0,
		'M', 'M', 0, 42, 0, 0, 0, 8, // TIFF header, first IFD at 8
		0, 1, // one entry
		0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, byte(orientation), 0, 0, // Orientation, SHORT, 1 value
		0, 0, 0, 0} // no next IFD
	out := make([]byte, 0, len(jpeg)+len(exif))
	return append(append(append(out, jpeg[:2]...), exif...), jpeg[2:]...)
}
