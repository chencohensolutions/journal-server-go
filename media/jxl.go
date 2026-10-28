package media

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"github.com/ebitengine/purego"
)

// JXL is libjxl loaded into the process, through purego like libvips (no cgo): it keeps a JPEG as JPEG XL without
// losing a bit, and makes the very same JPEG again from it. The JPEG's DCT coefficients are packed again with JPEG XL's
// better coding (about 20% smaller), and everything else the JPEG had (its markers, Exif, the order of its bytes) is
// kept in a "jbrd" box, so the JPEG comes back byte for byte. See docs/media.md, "JPEG XL".
type JXL struct {
	fn      map[string]uintptr
	Version string
}

var jxlFunctions = []string{
	"JxlEncoderVersion",
	"JxlEncoderCreate", "JxlEncoderDestroy", "JxlEncoderUseContainer", "JxlEncoderStoreJPEGMetadata",
	"JxlEncoderFrameSettingsCreate", "JxlEncoderFrameSettingsSetOption", "JxlEncoderAddJPEGFrame",
	"JxlEncoderCloseInput", "JxlEncoderProcessOutput", "JxlEncoderGetError",
	"JxlDecoderCreate", "JxlDecoderDestroy", "JxlDecoderSubscribeEvents", "JxlDecoderSetInput",
	"JxlDecoderCloseInput", "JxlDecoderProcessInput", "JxlDecoderSetJPEGBuffer", "JxlDecoderReleaseJPEGBuffer",
}

// Values of libjxl's enums (encode.h, decode.h).
const (
	jxlEncSuccess         = 0
	jxlEncNeedMoreOutput  = 2
	jxlEncSettingEffort   = 0 // JXL_ENC_FRAME_SETTING_EFFORT
	jxlDecSuccess         = 0
	jxlDecNeedImageOut    = 5
	jxlDecJPEGNeedMoreOut = 6
	jxlDecFullImage       = 0x1000
	jxlDecJPEGReconstruct = 0x2000
	jxlEffort             = 7 // cjxl's default: higher packs a JPEG's coefficients barely smaller, much slower
	jxlOutputStep         = 1 << 20
)

// ErrNoJXL means libjxl isn't loaded: JPEGs are kept as they are, and a JPEG XL made from one can't be checked.
var ErrNoJXL = errors.New("libjxl isn't loaded")

// LoadJXL loads libjxl from the first of libs that loads (JXLLibraries).
func LoadJXL(libs []string) (*JXL, error) {
	var errs []error
	for _, lib := range libs {
		handles, err := openVipsLibraries([]string{lib})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		x := &JXL{fn: map[string]uintptr{}}
		for _, name := range jxlFunctions {
			addr, err := findVipsSymbol(handles[0], name)
			if err != nil || addr == 0 {
				return nil, fmt.Errorf("%s has no %s", lib, name)
			}
			x.fn[name] = addr
		}
		v := x.call("JxlEncoderVersion")
		x.Version = fmt.Sprintf("%d.%d.%d", v/1000000, v/1000%1000, v%1000)
		return x, nil
	}
	return nil, fmt.Errorf("libjxl can't be loaded: %w", errors.Join(errs...))
}

//go:uintptrescapes
func (x *JXL) call(name string, args ...uintptr) uintptr {
	r, _, _ := purego.SyscallN(x.fn[name], args...)
	return r
}

// FromJPEG packs a JPEG as JPEG XL, losslessly. It doesn't check that the JPEG comes back the same: Keep does.
func (x *JXL) FromJPEG(jpeg []byte) ([]byte, error) {
	if x == nil {
		return nil, ErrNoJXL
	}
	if len(jpeg) == 0 {
		return nil, errors.New("an empty file")
	}
	var pin runtime.Pinner
	defer pin.Unpin()
	pin.Pin(&jpeg[0])
	enc := x.call("JxlEncoderCreate", 0)
	if enc == 0 {
		return nil, errors.New("JxlEncoderCreate failed")
	}
	defer x.call("JxlEncoderDestroy", enc)
	// A container, so the jbrd box (what makes the JPEG again) and the Exif box can be kept.
	x.call("JxlEncoderUseContainer", enc, 1)
	if x.call("JxlEncoderStoreJPEGMetadata", enc, 1) != jxlEncSuccess {
		return nil, errors.New("JxlEncoderStoreJPEGMetadata failed")
	}
	settings := x.call("JxlEncoderFrameSettingsCreate", enc, 0)
	x.call("JxlEncoderFrameSettingsSetOption", settings, jxlEncSettingEffort, jxlEffort)
	if x.call("JxlEncoderAddJPEGFrame", settings, uintptr(unsafe.Pointer(&jpeg[0])), uintptr(len(jpeg))) != jxlEncSuccess {
		return nil, fmt.Errorf("libjxl can't keep this JPEG losslessly (error %d)", x.call("JxlEncoderGetError", enc))
	}
	x.call("JxlEncoderCloseInput", enc)
	out := make([]byte, max(jxlOutputStep, len(jpeg)))
	used := 0
	for {
		pin.Pin(&out[0])
		next := uintptr(unsafe.Pointer(&out[used]))
		avail := uintptr(len(out) - used)
		status := x.call("JxlEncoderProcessOutput", enc, uintptr(unsafe.Pointer(&next)), uintptr(unsafe.Pointer(&avail)))
		used = len(out) - int(avail)
		switch status {
		case jxlEncSuccess:
			return out[:used], nil
		case jxlEncNeedMoreOutput:
			out = append(out, make([]byte, jxlOutputStep)...)
			out = out[:cap(out)]
		default:
			return nil, fmt.Errorf("libjxl failed packing the JPEG (error %d)", x.call("JxlEncoderGetError", enc))
		}
	}
}

// ToJPEG makes again the JPEG a JPEG XL was packed from (FromJPEG), byte for byte. A JPEG XL made some other way has
// no JPEG in it, which is an error.
func (x *JXL) ToJPEG(jxl []byte) ([]byte, error) {
	if x == nil {
		return nil, ErrNoJXL
	}
	if len(jxl) == 0 {
		return nil, errors.New("an empty file")
	}
	var pin runtime.Pinner
	defer pin.Unpin()
	pin.Pin(&jxl[0])
	dec := x.call("JxlDecoderCreate", 0)
	if dec == 0 {
		return nil, errors.New("JxlDecoderCreate failed")
	}
	defer x.call("JxlDecoderDestroy", dec)
	if x.call("JxlDecoderSubscribeEvents", dec, jxlDecJPEGReconstruct|jxlDecFullImage) != jxlDecSuccess {
		return nil, errors.New("JxlDecoderSubscribeEvents failed")
	}
	if x.call("JxlDecoderSetInput", dec, uintptr(unsafe.Pointer(&jxl[0])), uintptr(len(jxl))) != jxlDecSuccess {
		return nil, errors.New("JxlDecoderSetInput failed")
	}
	x.call("JxlDecoderCloseInput", dec)
	var out []byte
	used := 0
	give := func() {
		pin.Pin(&out[0])
		x.call("JxlDecoderSetJPEGBuffer", dec, uintptr(unsafe.Pointer(&out[used])), uintptr(len(out)-used))
	}
	for {
		switch status := x.call("JxlDecoderProcessInput", dec); status {
		case jxlDecJPEGReconstruct:
			out = make([]byte, max(jxlOutputStep, len(jxl)*5/4))
			give()
		case jxlDecJPEGNeedMoreOut:
			used = len(out) - int(x.call("JxlDecoderReleaseJPEGBuffer", dec))
			out = append(out, make([]byte, max(jxlOutputStep, len(out)/2))...)
			out = out[:cap(out)]
			give()
		case jxlDecFullImage:
			// The JPEG is written; the decoder says so once more with success.
		case jxlDecSuccess:
			if out == nil {
				return nil, errors.New("this JPEG XL has no JPEG in it")
			}
			used = len(out) - int(x.call("JxlDecoderReleaseJPEGBuffer", dec))
			return out[:used], nil
		case jxlDecNeedImageOut:
			return nil, errors.New("this JPEG XL has no JPEG in it")
		default:
			return nil, fmt.Errorf("libjxl failed making the JPEG again (status %d)", status)
		}
	}
}

// Keep packs a JPEG as JPEG XL (FromJPEG) and makes the JPEG again from it (ToJPEG); only when that is the very same
// JPEG, byte for byte, is the JPEG XL returned. A JPEG libjxl can't keep (CMYK, arithmetic coding, a damaged file)
// is an error, and stays a JPEG.
func (x *JXL) Keep(jpeg []byte) ([]byte, error) {
	jxl, err := x.FromJPEG(jpeg)
	if err != nil {
		return nil, err
	}
	again, err := x.ToJPEG(jxl)
	if err != nil {
		return nil, fmt.Errorf("checking the JPEG XL: %w", err)
	}
	if !bytes.Equal(again, jpeg) {
		return nil, errors.New("the JPEG made again from the JPEG XL isn't the same")
	}
	return jxl, nil
}

// IsJPEG reports whether a file is a JPEG, by its name or MIME type.
func IsJPEG(fileName, mimeType string) bool {
	switch strings.ToLower(filepath.Ext(fileName)) {
	case ".jpg", ".jpeg", ".jpe", ".jfif":
		return true
	}
	m := strings.ToLower(mimeType)
	return m == "image/jpeg" || m == "image/jpg" || m == "image/pjpeg"
}
