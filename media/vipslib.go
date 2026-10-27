package media

import (
	"errors"
	"fmt"
	"image"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// vipsLib is libvips loaded into the server: the library vips.exe itself runs, called through purego like ONNX
// Runtime (no cgo). Starting vips.exe or vipsheader.exe costs about 350 ms on Windows before any work, and what they
// write has to be read again; in here a picture is a few function calls. docs/media.md, "Previews".
//
// Only libvips's fixed-argument functions are called: purego can't call C functions that take a variable list of
// arguments on every system (vips_thumbnail(...) breaks on Apple Silicon). So an operation is made by name, its
// properties are set one by one (GLib GValues), and then it is built: the generic interface libvips's own bindings
// use. A crash in libvips takes the server with it; config.json "vips_as_program" keeps libvips out.
type vipsLib struct {
	fn map[string]uintptr
	// GLib's types of the values set as properties.
	imageType, sizeType, interestingType, blobType, arrayImageType uintptr
	Version                                                        string
}

// GLib's fundamental types (G_TYPE_INT and so on) are fixed numbers: the type's place shifted left by 2.
const (
	gTypeBoolean = 5 << 2
	gTypeInt     = 6 << 2
	gTypeString  = 16 << 2
)

// Values of libvips enums.
const (
	vipsSizeBoth     = 0 // VipsSize: shrink or enlarge to fit (1 is VIPS_SIZE_UP, only enlarge)
	vipsSizeDown     = 2 // only ever shrink
	vipsSizeForce    = 3 // exactly the size asked, the aspect not kept
	vipsCropCentre   = 1 // VipsInteresting: fill the size asked, cutting off the sides equally
	vipsFormatUchar  = 0 // VipsBandFormat
	vipsFormatUshort = 2
)

var vipsFunctions = []string{
	"vips_init", "vips_version", "vips_concurrency_set", "vips_cache_set_max", "vips_error_buffer", "vips_error_clear",
	"vips_thread_shutdown", "vips_operation_new", "vips_cache_operation_buildp", "vips_object_unref_outputs",
	"vips_object_set_from_string", "vips_foreign_find_load", "vips_image_get_as_string", "vips_foreign_find_save",
	"vips_image_get_type", "vips_size_get_type", "vips_interesting_get_type", "vips_blob_get_type",
	"vips_array_image_get_type",
	"vips_image_get_width", "vips_image_get_height", "vips_image_get_bands", "vips_image_get_format",
	"vips_image_get_int", "vips_image_set_int", "vips_image_remove", "vips_image_write_to_memory",
	"vips_image_copy_memory", "vips_image_new_from_memory_copy", "vips_blob_copy", "vips_array_image_new",
	"vips_area_unref",
	"g_value_init", "g_value_set_int", "g_value_set_boolean", "g_value_set_string", "g_value_set_enum",
	"g_value_set_object", "g_value_set_boxed", "g_value_dup_object", "g_value_unset",
	"g_object_set_property", "g_object_get_property", "g_object_unref", "g_free",
}

// loadVipsLib loads libvips from the files libs (the library and, on Windows, the GLib libraries beside it), starts
// it, and lets it use up to threads threads for each operation.
func loadVipsLib(libs []string, threads int) (*vipsLib, error) {
	handles, err := openVipsLibraries(libs)
	if err != nil {
		return nil, err
	}
	l := &vipsLib{fn: map[string]uintptr{}}
	for _, name := range vipsFunctions {
		for _, h := range handles {
			if addr, err := findVipsSymbol(h, name); err == nil && addr != 0 {
				l.fn[name] = addr
				break
			}
		}
		if l.fn[name] == 0 {
			return nil, fmt.Errorf("%s has no %s", libs[0], name)
		}
	}
	name := cStr("journal")
	if l.call("vips_init", uintptr(unsafe.Pointer(&name[0]))) != 0 {
		return nil, errors.New("vips_init: " + l.lastError())
	}
	major, minor, micro := l.call("vips_version", 0), l.call("vips_version", 1), l.call("vips_version", 2)
	l.Version = fmt.Sprintf("%d.%d.%d", major, minor, micro)
	// keep=icc (the save options previews use) and the operations here are all there since 8.15.
	if int(major)*100+int(minor) < 815 {
		return nil, fmt.Errorf("libvips %s is too old (8.15 or later is needed)", l.Version)
	}
	l.call("vips_concurrency_set", uintptr(max(1, threads)))
	// Nothing is made twice the same: keeping operations would only hold memory.
	l.call("vips_cache_set_max", 0)
	l.imageType, l.sizeType = l.call("vips_image_get_type"), l.call("vips_size_get_type")
	l.interestingType = l.call("vips_interesting_get_type")
	l.blobType, l.arrayImageType = l.call("vips_blob_get_type"), l.call("vips_array_image_get_type")
	return l, nil
}

// call runs the libvips or GLib function name with args: integers, and pointers converted in the call itself.
//
//go:uintptrescapes
func (l *vipsLib) call(name string, args ...uintptr) uintptr {
	r, _, _ := purego.SyscallN(l.fn[name], args...)
	return r
}

// lastError takes libvips's error message for this thread, and clears it.
func (l *vipsLib) lastError() string {
	message := goStr(l.call("vips_error_buffer"))
	l.call("vips_error_clear")
	if message == "" {
		return "unknown error"
	}
	return message
}

// onThread runs fn on one OS thread, which libvips is told it is done with at the end (it keeps buffers per
// thread). Every use of libvips goes through it; other goroutines (requests) carry on on other threads meanwhile.
func (l *vipsLib) onThread(fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer l.call("vips_thread_shutdown")
	return fn()
}

// vipsImage is a VipsImage we hold a reference to. Every one must be Released: a decoded original is large, and
// the garbage collector doesn't see C memory.
type vipsImage struct {
	l       *vipsLib
	handle  uintptr
	release sync.Once
}

func (l *vipsLib) image(handle uintptr) *vipsImage { return &vipsImage{l: l, handle: handle} }

// Release drops the reference; img can't be used after.
func (img *vipsImage) Release() {
	img.release.Do(func() {
		img.l.call("g_object_unref", img.handle)
		img.handle = 0
	})
}

func (img *vipsImage) size() (width, height int) {
	return int(int32(img.l.call("vips_image_get_width", img.handle))), int(int32(img.l.call("vips_image_get_height", img.handle)))
}

// orientation is the image's EXIF orientation, 1 to 8; 1 when it has none.
func (img *vipsImage) orientation() int {
	name, n := cStr("orientation"), new(int32)
	if img.l.call("vips_image_get_int", img.handle, uintptr(unsafe.Pointer(&name[0])), uintptr(unsafe.Pointer(n))) != 0 {
		img.l.call("vips_error_clear")
		return 1
	}
	if *n < 1 || *n > 8 {
		return 1
	}
	return int(*n)
}

// operation is one libvips operation being set up; the first error sticks, and build reports it.
type operation struct {
	l      *vipsLib
	name   string
	handle uintptr
	err    error
}

func (l *vipsLib) operation(name string) *operation {
	chars := cStr(name)
	o := &operation{l: l, name: name, handle: l.call("vips_operation_new", uintptr(unsafe.Pointer(&chars[0])))}
	if o.handle == 0 {
		o.err = fmt.Errorf("vips %s: %s", name, l.lastError())
	}
	return o
}

func (o *operation) set(property string, gtype uintptr, setter string, value uintptr) *operation {
	if o.err != nil {
		return o
	}
	v, name := new(gvalue), cStr(property)
	o.l.call("g_value_init", uintptr(unsafe.Pointer(v)), gtype)
	o.l.call(setter, uintptr(unsafe.Pointer(v)), value)
	o.l.call("g_object_set_property", o.handle, uintptr(unsafe.Pointer(&name[0])), uintptr(unsafe.Pointer(v)))
	o.l.call("g_value_unset", uintptr(unsafe.Pointer(v)))
	return o
}

func (o *operation) setInt(property string, n int) *operation {
	return o.set(property, gTypeInt, "g_value_set_int", uintptr(n))
}

func (o *operation) setBool(property string, b bool) *operation {
	n := uintptr(0)
	if b {
		n = 1
	}
	return o.set(property, gTypeBoolean, "g_value_set_boolean", n)
}

func (o *operation) setEnum(property string, gtype uintptr, n int) *operation {
	return o.set(property, gtype, "g_value_set_enum", uintptr(n))
}

func (o *operation) setImage(property string, img *vipsImage) *operation {
	return o.set(property, o.l.imageType, "g_value_set_object", img.handle) // g_value_set_object takes a reference
}

func (o *operation) setString(property, s string) *operation {
	if o.err != nil {
		return o
	}
	v, name, chars := new(gvalue), cStr(property), cStr(s)
	o.l.call("g_value_init", uintptr(unsafe.Pointer(v)), gTypeString)
	o.l.call("g_value_set_string", uintptr(unsafe.Pointer(v)), uintptr(unsafe.Pointer(&chars[0]))) // copied
	o.l.call("g_object_set_property", o.handle, uintptr(unsafe.Pointer(&name[0])), uintptr(unsafe.Pointer(v)))
	o.l.call("g_value_unset", uintptr(unsafe.Pointer(v)))
	return o
}

// setOptions sets properties from libvips's option text, as a file name's "[Q=85,keep=icc]" would.
func (o *operation) setOptions(options string) *operation {
	if o.err != nil || options == "" {
		return o
	}
	chars := cStr(options)
	if o.l.call("vips_object_set_from_string", o.handle, uintptr(unsafe.Pointer(&chars[0]))) != 0 {
		o.err = fmt.Errorf("vips %s %s: %s", o.name, options, o.l.lastError())
	}
	return o
}

// run builds the operation and returns its image output named out (nil for a saver: out ""), then lets the
// operation go.
func (o *operation) run(out string) (*vipsImage, error) {
	if o.err != nil {
		if o.handle != 0 {
			o.l.call("g_object_unref", o.handle)
		}
		return nil, o.err
	}
	handle := new(uintptr)
	*handle = o.handle
	if o.l.call("vips_cache_operation_buildp", uintptr(unsafe.Pointer(handle))) != 0 {
		err := fmt.Errorf("vips %s: %s", o.name, o.l.lastError())
		o.l.call("vips_object_unref_outputs", *handle)
		o.l.call("g_object_unref", *handle)
		return nil, err
	}
	defer func() {
		o.l.call("vips_object_unref_outputs", *handle)
		o.l.call("g_object_unref", *handle)
	}()
	if out == "" {
		return nil, nil
	}
	v, name := new(gvalue), cStr(out)
	o.l.call("g_value_init", uintptr(unsafe.Pointer(v)), o.l.imageType)
	o.l.call("g_object_get_property", *handle, uintptr(unsafe.Pointer(&name[0])), uintptr(unsafe.Pointer(v)))
	img := o.l.call("g_value_dup_object", uintptr(unsafe.Pointer(v)))
	o.l.call("g_value_unset", uintptr(unsafe.Pointer(v)))
	if img == 0 {
		return nil, fmt.Errorf("vips %s: no %s", o.name, out)
	}
	return o.l.image(img), nil
}

// gvalue is GLib's GValue: its type, then two words of data. Zeroed, it is ready for g_value_init.
type gvalue [3]uint64

// load opens the file at path with the loader libvips picks for it (by its first bytes). Only the header is read;
// the pixels when something needs them.
func (l *vipsLib) load(path string) (*vipsImage, error) {
	chars := cStr(path)
	loader := goStr(l.call("vips_foreign_find_load", uintptr(unsafe.Pointer(&chars[0]))))
	if loader == "" {
		return nil, fmt.Errorf("libvips can't read %s: %s", path, l.lastError())
	}
	return l.operation(loader).setString("filename", path).run("out")
}

// thumbnailFile is the picture in the file at path, upright, resized to width×height as size says, the
// fastest way libvips has: a JPEG is decoded already shrunk.
func (l *vipsLib) thumbnailFile(path string, width, height, size int) (*vipsImage, error) {
	return l.operation("thumbnail").setString("filename", path).setInt("width", width).setInt("height", height).
		setEnum("size", l.sizeType, size).run("out")
}

// thumbnail is img, upright, resized to width×height as size says.
func (l *vipsLib) thumbnail(img *vipsImage, width, height, size int) (*vipsImage, error) {
	return l.operation("thumbnail_image").setImage("in", img).setInt("width", width).setInt("height", height).
		setEnum("size", l.sizeType, size).run("out")
}

// thumbnailCover is img, upright, filling exactly width×height: scaled so the box is covered, the rest cut off
// equally on both sides.
func (l *vipsLib) thumbnailCover(img *vipsImage, width, height int) (*vipsImage, error) {
	return l.operation("thumbnail_image").setImage("in", img).setInt("width", width).setInt("height", height).
		setEnum("crop", l.interestingType, vipsCropCentre).run("out")
}

// autorot is img turned upright by its EXIF orientation (which it then no longer has).
func (l *vipsLib) autorot(img *vipsImage) (*vipsImage, error) {
	return l.operation("autorot").setImage("in", img).run("out")
}

func (l *vipsLib) extractArea(img *vipsImage, left, top, width, height int) (*vipsImage, error) {
	return l.operation("extract_area").setImage("input", img).setInt("left", left).setInt("top", top).
		setInt("width", width).setInt("height", height).run("out")
}

// save writes img to path in the format its extension names, with libvips's option text options.
func (l *vipsLib) save(img *vipsImage, path, options string) error {
	chars := cStr(path)
	saver := goStr(l.call("vips_foreign_find_save", uintptr(unsafe.Pointer(&chars[0]))))
	if saver == "" {
		return fmt.Errorf("libvips can't write %s: %s", path, l.lastError())
	}
	_, err := l.operation(saver).setImage("in", img).setString("filename", path).setOptions(options).run("")
	return err
}

// inMemory is img decoded whole into memory, so that it is never decoded again however often it is used.
func (l *vipsLib) inMemory(img *vipsImage) (*vipsImage, error) {
	h := l.call("vips_image_copy_memory", img.handle)
	if h == 0 {
		return nil, errors.New("vips copy_memory: " + l.lastError())
	}
	return l.image(h), nil
}

// fromMemory is an image of the pixels (copied): width×height, bands samples each, of format.
func (l *vipsLib) fromMemory(pixels unsafe.Pointer, size uintptr, width, height, bands, format int) (*vipsImage, error) {
	h := l.call("vips_image_new_from_memory_copy", uintptr(pixels), size, uintptr(width), uintptr(height),
		uintptr(bands), uintptr(format))
	if h == 0 {
		return nil, errors.New("vips new_from_memory_copy: " + l.lastError())
	}
	return l.image(h), nil
}

// setOrientation sets img's EXIF orientation, which thumbnail turns it by.
func (l *vipsLib) setOrientation(img *vipsImage, orientation int) {
	name := cStr("orientation")
	l.call("vips_image_set_int", img.handle, uintptr(unsafe.Pointer(&name[0])), uintptr(orientation))
}

// loadJXL decodes a JPEG XL stream.
func (l *vipsLib) loadJXL(data []byte) (*vipsImage, error) {
	blob := l.call("vips_blob_copy", uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)))
	if blob == 0 {
		return nil, errors.New("vips blob: " + l.lastError())
	}
	defer l.call("vips_area_unref", blob)
	return l.operation("jxlload_buffer").set("buffer", l.blobType, "g_value_set_boxed", blob).run("out")
}

// arrayJoin lays images out in a grid, across to a row.
func (l *vipsLib) arrayJoin(images []*vipsImage, across int) (*vipsImage, error) {
	handles := make([]uintptr, len(images))
	for i, img := range images {
		handles[i] = img.handle
	}
	array := l.call("vips_array_image_new", uintptr(unsafe.Pointer(&handles[0])), uintptr(len(handles)))
	runtime.KeepAlive(images)
	if array == 0 {
		return nil, errors.New("vips array: " + l.lastError())
	}
	defer l.call("vips_area_unref", array)
	return l.operation("arrayjoin").set("in", l.arrayImageType, "g_value_set_boxed", array).setInt("across", across).run("out")
}

// rgba copies img's pixels into an RGBA picture: grey is spread to the three colours, 16-bit is cut to 8, and a
// transparent picture is laid on black, as vips.exe does when it saves a JPEG.
func (l *vipsLib) rgba(img *vipsImage) (*image.RGBA, error) {
	w, h := img.size()
	bands, format := int(int32(l.call("vips_image_get_bands", img.handle))), int(int32(l.call("vips_image_get_format", img.handle)))
	if w < 1 || h < 1 || bands < 1 || bands > 4 || (format != vipsFormatUchar && format != vipsFormatUshort) {
		return nil, fmt.Errorf("libvips gave a %d×%d picture of %d bands in format %d", w, h, bands, format)
	}
	raw, free, err := l.memory(img, w*h*bands*sampleSize(format))
	if err != nil {
		return nil, err
	}
	defer free()
	at := func(i int) int { // sample i, 0 to 255
		if format == vipsFormatUshort {
			return int(*(*uint16)(unsafe.Pointer(&raw[2*i]))) >> 8
		}
		return int(raw[i])
	}
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range w * h {
		var r, g, b int
		if bands <= 2 {
			r = at(i * bands)
			g, b = r, r
		} else {
			r, g, b = at(i*bands), at(i*bands+1), at(i*bands+2)
		}
		if bands == 2 || bands == 4 {
			a := at(i*bands + bands - 1)
			r, g, b = r*a/255, g*a/255, b*a/255
		}
		rgba.Pix[4*i], rgba.Pix[4*i+1], rgba.Pix[4*i+2], rgba.Pix[4*i+3] = uint8(r), uint8(g), uint8(b), 255
	}
	return rgba, nil
}

// rgb16 copies a 3-band 16-bit image's samples.
func (l *vipsLib) rgb16(img *vipsImage) (width, height int, pixels []uint16, err error) {
	w, h := img.size()
	bands, format := int(int32(l.call("vips_image_get_bands", img.handle))), int(int32(l.call("vips_image_get_format", img.handle)))
	if bands != 3 || format != vipsFormatUshort {
		return 0, 0, nil, fmt.Errorf("decoded raw data has %d bands in format %d, not 3 of 16 bits", bands, format)
	}
	raw, free, err := l.memory(img, w*h*3*2)
	if err != nil {
		return 0, 0, nil, err
	}
	defer free()
	pixels = make([]uint16, w*h*3)
	copy(pixels, unsafe.Slice((*uint16)(unsafe.Pointer(&raw[0])), len(pixels)))
	return w, h, pixels, nil
}

// memory runs img and returns its pixels, which must be want bytes, and free, to let them go.
func (l *vipsLib) memory(img *vipsImage, want int) (raw []byte, free func(), err error) {
	size := new(uintptr)
	data := l.call("vips_image_write_to_memory", img.handle, uintptr(unsafe.Pointer(size)))
	if data == 0 {
		return nil, nil, errors.New("vips write_to_memory: " + l.lastError())
	}
	free = func() { l.call("g_free", data) }
	if int(*size) != want {
		free()
		return nil, nil, fmt.Errorf("libvips gave %d bytes, not %d", *size, want)
	}
	return unsafe.Slice((*byte)(cPointer(data)), *size), free, nil
}

func sampleSize(format int) int {
	if format == vipsFormatUshort {
		return 2
	}
	return 1
}

func cStr(s string) []byte { return append([]byte(s), 0) }

// cPointer turns an address libvips returned (C memory, never moved) into a pointer.
func cPointer(address uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&address))
}

// goStr copies the C string at address.
func goStr(address uintptr) string {
	if address == 0 {
		return ""
	}
	p := cPointer(address)
	n := 0
	for *(*byte)(unsafe.Add(p, n)) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(p), n))
}
