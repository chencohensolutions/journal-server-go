package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"strings"
)

// isDNG reports whether a stored original is a DNG (RAW) file, by its name.
func isDNG(name string) bool { return strings.EqualFold(path.Ext(name), ".dng") }

// Compression of DNG 1.7 image data with JPEG XL, and the photometric interpretation of demosaiced
// ("linear") raw data, as Samsung's Expert RAW and Pro mode write them.
const (
	compressionJPEGXL = 52546
	photometricLinear = 34892
)

// linearDNG is what developing a DNG's demosaiced, JPEG XL compressed raw data needs: where the data
// is, and the colour processing the DNG specification describes for it.
type linearDNG struct {
	width, height int
	// The image data: one JPEG XL stream per tile (or one for the whole image, tileWidth 0).
	pieces                     []section
	tileWidth, tileHeight      int
	orientation                int
	black, white               [3]float64
	colorMatrix                [2]*matrix3 // XYZ → camera, per calibration illuminant
	forwardMatrix              [2]*matrix3 // white-balanced camera → XYZ D50
	cameraCalibration          [2]*matrix3
	illuminant                 [2]int
	analogBalance              [3]float64
	asShotNeutral              *[3]float64
	asShotWhiteXY              *[2]float64
	baselineExposure           float64
	gainTable                  *gainTableMap
	cropX, cropY, cropW, cropH int
}

type section struct{ offset, length int64 }

// gainTableMap is DNG 1.7's ProfileGainTableMap: per point of a grid over the image, a gain for each
// brightness, so shadows are lifted without lifting highlights (local tone mapping).
type gainTableMap struct {
	pointsV, pointsH, pointsN int
	spacingV, spacingH        float64
	originV, originH          float64
	weights                   [5]float64 // of R, G, B, min and max, giving the brightness
	gains                     []float32  // [v][h][n]
}

// TIFF and DNG tags read from a DNG.
const (
	tagNewSubfileType     = 254
	tagWidth              = 256
	tagHeight             = 257
	tagCompression        = 259
	tagPhotometric        = 262
	tagStripOffsets       = 273
	tagOrientation        = 274
	tagSamplesPerPixel    = 277
	tagStripCounts        = 279
	tagTileWidth          = 322
	tagTileLength         = 323
	tagTileOffsets        = 324
	tagTileCounts         = 325
	tagSubIFDs            = 330
	tagBlackLevel         = 50714
	tagWhiteLevel         = 50717
	tagDefaultCropOrigin  = 50719
	tagDefaultCropSize    = 50720
	tagColorMatrix1       = 50721
	tagColorMatrix2       = 50722
	tagCameraCalibration1 = 50723
	tagCameraCalibration2 = 50724
	tagAnalogBalance      = 50727
	tagAsShotNeutral      = 50728
	tagAsShotWhiteXY      = 50729
	tagBaselineExposure   = 50730
	tagIlluminant1        = 50778
	tagIlluminant2        = 50779
	tagForwardMatrix1     = 50964
	tagForwardMatrix2     = 50965
	tagGainTableMap       = 52525
)

// tiffValue is one tag's values, as numbers, and its raw bytes (for undefined data).
type tiffValue struct {
	numbers []float64
	raw     []byte
}

// readLinearDNG reads the main image of a DNG whose raw data is demosaiced and JPEG XL compressed. It
// returns nil (and no error) for other DNGs, which libvips (LibRaw) reads itself.
func readLinearDNG(file string) (*linearDNG, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	header := make([]byte, 8)
	if _, err := f.ReadAt(header, 0); err != nil {
		return nil, err
	}
	var order binary.ByteOrder
	switch string(header[:4]) {
	case "II*\x00":
		order = binary.LittleEndian
	case "MM\x00*":
		order = binary.BigEndian
	default:
		return nil, errors.New("not a TIFF/DNG file")
	}

	readIFD := func(offset int64) (map[uint16]tiffValue, []uint32, error) {
		count := make([]byte, 2)
		if _, err := f.ReadAt(count, offset); err != nil {
			return nil, nil, err
		}
		n := int(order.Uint16(count))
		entries := make([]byte, n*12)
		if _, err := f.ReadAt(entries, offset+2); err != nil {
			return nil, nil, err
		}
		tags := map[uint16]tiffValue{}
		var subIFDs []uint32
		for i := 0; i < n; i++ {
			e := entries[i*12:]
			tag, typ, num := order.Uint16(e), order.Uint16(e[2:]), order.Uint32(e[4:])
			size := map[uint16]uint32{1: 1, 3: 2, 4: 4, 5: 8, 7: 1, 9: 4, 10: 8, 11: 4, 12: 8, 13: 4}[typ]
			if size == 0 || num == 0 || uint64(size)*uint64(num) > 1<<24 {
				continue
			}
			raw := e[8:12]
			if size*num > 4 {
				raw = make([]byte, size*num)
				if _, err := f.ReadAt(raw, int64(order.Uint32(e[8:]))); err != nil {
					return nil, nil, fmt.Errorf("tag %d: %w", tag, err)
				}
			} else {
				raw = raw[:size*num]
			}
			value := tiffValue{raw: raw}
			if typ != 7 && num <= 4096 {
				value.numbers = make([]float64, num)
				for j := range value.numbers {
					b := raw[j*int(size):]
					switch typ {
					case 1:
						value.numbers[j] = float64(b[0])
					case 3:
						value.numbers[j] = float64(order.Uint16(b))
					case 4, 13:
						value.numbers[j] = float64(order.Uint32(b))
					case 9:
						value.numbers[j] = float64(int32(order.Uint32(b)))
					case 5:
						value.numbers[j] = float64(order.Uint32(b)) / float64(order.Uint32(b[4:]))
					case 10:
						value.numbers[j] = float64(int32(order.Uint32(b))) / float64(int32(order.Uint32(b[4:])))
					case 11:
						value.numbers[j] = float64(math.Float32frombits(order.Uint32(b)))
					case 12:
						value.numbers[j] = math.Float64frombits(order.Uint64(b))
					}
				}
			}
			tags[tag] = value
			if tag == tagSubIFDs {
				for _, v := range value.numbers {
					subIFDs = append(subIFDs, uint32(v))
				}
			}
		}
		return tags, subIFDs, nil
	}

	ifd0, subIFDs, err := readIFD(int64(order.Uint32(header[4:])))
	if err != nil {
		return nil, err
	}
	// The main image is IFD0 or one of its SubIFDs: the one not marked as a reduced-resolution preview.
	main := ifd0
	if int(first(ifd0, tagNewSubfileType, 0))&1 == 1 {
		main = nil
		for _, offset := range subIFDs {
			sub, _, err := readIFD(int64(offset))
			if err != nil {
				return nil, err
			}
			if int(first(sub, tagNewSubfileType, 0))&1 == 0 {
				main = sub
				break
			}
		}
	}
	if main == nil || first(main, tagCompression, 0) != compressionJPEGXL ||
		first(main, tagPhotometric, 0) != photometricLinear || first(main, tagSamplesPerPixel, 1) != 3 {
		return nil, nil
	}

	d := &linearDNG{
		width:            int(first(main, tagWidth, 0)),
		height:           int(first(main, tagHeight, 0)),
		orientation:      int(first(ifd0, tagOrientation, 1)),
		illuminant:       [2]int{int(first(ifd0, tagIlluminant1, 0)), int(first(ifd0, tagIlluminant2, 0))},
		baselineExposure: first(ifd0, tagBaselineExposure, 0),
		analogBalance:    [3]float64{1, 1, 1},
	}
	if d.orientation < 1 || d.orientation > 8 {
		d.orientation = 1
	}
	if d.width <= 0 || d.height <= 0 {
		return nil, errors.New("DNG without a size")
	}
	offsets, counts := main[tagStripOffsets].numbers, main[tagStripCounts].numbers
	if tw := int(first(main, tagTileWidth, 0)); tw > 0 {
		d.tileWidth, d.tileHeight = tw, int(first(main, tagTileLength, 0))
		offsets, counts = main[tagTileOffsets].numbers, main[tagTileCounts].numbers
		across, down := (d.width+tw-1)/tw, (d.height+d.tileHeight-1)/max(1, d.tileHeight)
		if d.tileHeight <= 0 || len(offsets) != across*down {
			return nil, errors.New("DNG tiles don't cover the image")
		}
	} else if len(offsets) != 1 {
		return nil, fmt.Errorf("DNG image in %d strips", len(offsets))
	}
	if len(offsets) == 0 || len(offsets) != len(counts) {
		return nil, errors.New("DNG image data not found")
	}
	for i := range offsets {
		d.pieces = append(d.pieces, section{int64(offsets[i]), int64(counts[i])})
	}

	// Levels are per sample (repeated patterns of black levels are averaged).
	whites := main[tagWhiteLevel].numbers
	if len(whites) == 0 {
		whites = ifd0[tagWhiteLevel].numbers
	}
	blacks := main[tagBlackLevel].numbers
	if len(blacks) == 0 {
		blacks = ifd0[tagBlackLevel].numbers
	}
	for c := 0; c < 3; c++ {
		d.white[c] = 65535
		if len(whites) > 0 {
			d.white[c] = whites[min(c, len(whites)-1)]
		}
		if len(blacks) > 0 {
			var sum float64
			var n int
			for i := c % len(blacks); i < len(blacks); i += max(1, min(3, len(blacks))) {
				sum += blacks[i]
				n++
			}
			d.black[c] = sum / float64(n)
		}
	}
	if crop := main[tagDefaultCropSize].numbers; len(crop) == 2 {
		origin := main[tagDefaultCropOrigin].numbers
		if len(origin) != 2 {
			origin = []float64{0, 0}
		}
		d.cropX, d.cropY, d.cropW, d.cropH = int(origin[0]), int(origin[1]), int(crop[0]), int(crop[1])
	}
	if d.cropW <= 0 || d.cropH <= 0 || d.cropX+d.cropW > d.width || d.cropY+d.cropH > d.height {
		d.cropX, d.cropY, d.cropW, d.cropH = 0, 0, d.width, d.height
	}

	matrix := func(tag uint16) *matrix3 {
		if v := ifd0[tag].numbers; len(v) == 9 {
			m := matrix3(v)
			return &m
		}
		return nil
	}
	d.colorMatrix = [2]*matrix3{matrix(tagColorMatrix1), matrix(tagColorMatrix2)}
	d.forwardMatrix = [2]*matrix3{matrix(tagForwardMatrix1), matrix(tagForwardMatrix2)}
	d.cameraCalibration = [2]*matrix3{matrix(tagCameraCalibration1), matrix(tagCameraCalibration2)}
	if d.colorMatrix[0] == nil {
		return nil, errors.New("DNG without a colour matrix")
	}
	if v := ifd0[tagAnalogBalance].numbers; len(v) == 3 {
		d.analogBalance = [3]float64(v)
	}
	if v := ifd0[tagAsShotNeutral].numbers; len(v) == 3 {
		n := [3]float64(v)
		d.asShotNeutral = &n
	} else if v := ifd0[tagAsShotWhiteXY].numbers; len(v) == 2 {
		xy := [2]float64(v)
		d.asShotWhiteXY = &xy
	}
	if raw := ifd0[tagGainTableMap].raw; raw != nil {
		if d.gainTable, err = parseGainTableMap(raw, order); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func first(tags map[uint16]tiffValue, tag uint16, fallback float64) float64 {
	if v := tags[tag].numbers; len(v) > 0 {
		return v[0]
	}
	return fallback
}

// parseGainTableMap reads ProfileGainTableMap: points (V, H), spacing and origin as fractions of the
// image, points per brightness, the input weights, then the gains.
func parseGainTableMap(raw []byte, order binary.ByteOrder) (*gainTableMap, error) {
	if len(raw) < 64 {
		return nil, errors.New("ProfileGainTableMap too short")
	}
	g := &gainTableMap{
		pointsV:  int(order.Uint32(raw)),
		pointsH:  int(order.Uint32(raw[4:])),
		spacingV: math.Float64frombits(order.Uint64(raw[8:])),
		spacingH: math.Float64frombits(order.Uint64(raw[16:])),
		originV:  math.Float64frombits(order.Uint64(raw[24:])),
		originH:  math.Float64frombits(order.Uint64(raw[32:])),
		pointsN:  int(order.Uint32(raw[40:])),
	}
	for i := range g.weights {
		g.weights[i] = float64(math.Float32frombits(order.Uint32(raw[44+i*4:])))
	}
	count := g.pointsV * g.pointsH * g.pointsN
	if g.pointsV < 1 || g.pointsH < 1 || g.pointsN < 2 || g.spacingV <= 0 || g.spacingH <= 0 || len(raw) < 64+count*4 {
		return nil, errors.New("ProfileGainTableMap is damaged")
	}
	g.gains = make([]float32, count)
	for i := range g.gains {
		g.gains[i] = math.Float32frombits(order.Uint32(raw[64+i*4:]))
	}
	return g, nil
}

// gain is the table's gain at (v, h), fractions of the image's height and width, for brightness b
// (0–1), interpolated between the nearest points.
func (g *gainTableMap) gain(v, h, b float64) float64 {
	fv := math.Max(0, math.Min(float64(g.pointsV-1), (v-g.originV)/g.spacingV))
	fh := math.Max(0, math.Min(float64(g.pointsH-1), (h-g.originH)/g.spacingH))
	fn := math.Max(0, math.Min(1, b)) * float64(g.pointsN-1)
	v0, h0, n0 := int(fv), int(fh), int(fn)
	v1, h1, n1 := min(v0+1, g.pointsV-1), min(h0+1, g.pointsH-1), min(n0+1, g.pointsN-1)
	dv, dh, dn := fv-float64(v0), fh-float64(h0), fn-float64(n0)
	at := func(v, h, n int) float64 { return float64(g.gains[(v*g.pointsH+h)*g.pointsN+n]) }
	lerp := func(a, b, t float64) float64 { return a + (b-a)*t }
	point := func(v, h int) float64 { return lerp(at(v, h, n0), at(v, h, n1), dn) }
	return lerp(lerp(point(v0, h0), point(v0, h1), dh), lerp(point(v1, h0), point(v1, h1), dh), dv)
}

// brightness is the table's input for a linear RGB value.
func (g *gainTableMap) brightness(r, gr, b float64) float64 {
	w := g.weights
	return w[0]*r + w[1]*gr + w[2]*b + w[3]*min(r, gr, b) + w[4]*max(r, gr, b)
}
