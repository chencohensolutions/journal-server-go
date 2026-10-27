package media

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"unsafe"
)

// Camera is the camera that took a photo or video, as its tags say: EXIF Make and Model for a photo
// ("samsung", "SM-S931B"; "Canon", "Canon EOS 80D"), Android's or QuickTime's tags for a video. Either may be empty.
type Camera struct {
	Make  string `json:"make,omitempty"`
	Model string `json:"model,omitempty"`
}

// key is how cameras are told apart: make and model ignoring case and spaces around them.
func (c Camera) key() string {
	return strings.ToLower(strings.TrimSpace(c.Make)) + "\x00" + strings.ToLower(strings.TrimSpace(c.Model))
}

// newCamera is the camera of make and model as tags give them (trimmed, without NULs), nil when both are empty.
func newCamera(maker, model string) *Camera {
	clean := func(s string) string { return strings.TrimSpace(strings.Trim(s, "\x00 ")) }
	c := Camera{Make: clean(maker), Model: clean(model)}
	if c.Make == "" && c.Model == "" {
		return nil
	}
	return &c
}

// CameraTools are what ReadCamera reads tags with: libvips in the server (or else the vipsheader program) for
// photos, ffprobe for videos. The zero value reads only DNGs (TiffExif).
type CameraTools struct {
	lib                 *vipsLib
	vipsheader, ffprobe string
}

// CameraTools are the tools the previews use, for ReadCamera: libvips in the server when it is loaded, vipsheader
// only without it ("vips_as_program"), and ffprobe.
func (p *Previews) CameraTools() CameraTools {
	return CameraTools{lib: p.lib, vipsheader: p.vipsheader, ffprobe: p.ffprobe}
}

// WithFFprobe is t reading videos with ffprobe, when t has no ffprobe of its own.
func (t CameraTools) WithFFprobe(ffprobe string) CameraTools {
	if t.ffprobe == "" {
		t.ffprobe = ffprobe
	}
	return t
}

// ReadCamera reads which camera took the photo or video at path, from its tags; nil when it has none, or they can't
// be read (no libvips, no ffprobe, a sound recording). A photo's header is read by libvips in the server (a few
// milliseconds), a DNG's EXIF from the file itself (TiffExif), and only with "vips_as_program" by vipsheader (about
// 350 ms to start on Windows). It is slow next to a turn: callers read it before theirs, never inside one.
func ReadCamera(ctx context.Context, tools CameraTools, path string, kind Kind) *Camera {
	switch kind {
	case KindAudio:
		return nil
	case KindVideo:
		return videoCamera(ctx, tools.ffprobe, path)
	}
	if exif, err := TiffExif(path); err == nil {
		if c := newCamera(exif["exif-ifd0-Make"], exif["exif-ifd0-Model"]); c != nil {
			return c
		}
	}
	if tools.lib != nil {
		var c *Camera
		err := tools.lib.onThread(func() error {
			img, err := tools.lib.load(path)
			if err != nil {
				return err
			}
			defer img.Release()
			maker, _ := img.getString("exif-ifd0-Make")
			model, _ := img.getString("exif-ifd0-Model")
			c = newCamera(exifText(maker), exifText(model))
			return nil
		})
		if err == nil {
			return c
		}
	}
	if tools.vipsheader != "" {
		out, err := exec.CommandContext(ctx, tools.vipsheader, "-a", path).Output()
		if err != nil {
			return nil
		}
		fields := map[string]string{}
		for line := range strings.SplitSeq(string(out), "\n") {
			if name, value, ok := strings.Cut(strings.TrimRight(line, "\r"), ": "); ok {
				fields[name] = exifText(value)
			}
		}
		return newCamera(fields["exif-ifd0-Make"], fields["exif-ifd0-Model"])
	}
	return nil
}

// exifText is an EXIF field's value as libvips gives it, without its description: "samsung (samsung, ASCII, 8
// components, 8 bytes)" → "samsung".
func exifText(value string) string {
	text, _, _ := strings.Cut(value, " (")
	return strings.TrimSpace(text)
}

// videoCamera reads a video's camera from its tags with ffprobe (CameraFromTags).
func videoCamera(ctx context.Context, ffprobe, path string) *Camera {
	if ffprobe == "" {
		return nil
	}
	out, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_entries", "format_tags", "-of", "json", path).Output()
	if err != nil {
		return nil
	}
	var probe struct {
		Format struct {
			Tags map[string]string `json:"tags"`
		} `json:"format"`
	}
	if json.Unmarshal(out, &probe) != nil {
		return nil
	}
	return CameraFromTags(probe.Format.Tags)
}

// CameraFromTags is a video's camera from its container's tags as ffprobe shows them: Android's
// com.android.manufacturer and com.android.model, QuickTime's com.apple.quicktime.make and .model, or plain make and
// model. Nil without any.
func CameraFromTags(tags map[string]string) *Camera {
	lower := map[string]string{}
	for k, v := range tags {
		lower[strings.ToLower(k)] = v
	}
	first := func(keys ...string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(lower[k]); v != "" {
				return v
			}
		}
		return ""
	}
	return newCamera(first("com.android.manufacturer", "com.apple.quicktime.make", "make"),
		first("com.android.model", "com.apple.quicktime.model", "model"))
}

// CameraFromEXIF is a photo's camera from its EXIF fields as vipsheader names them ("exif-ifd0-Make",
// "exif-ifd0-Model", their descriptions cut off already); nil without either.
func CameraFromEXIF(fields map[string]string) *Camera {
	return newCamera(exifText(fields["exif-ifd0-Make"]), exifText(fields["exif-ifd0-Model"]))
}

// getString is the image's field name as text (vips_image_get_as_string); false when it has none.
func (img *vipsImage) getString(name string) (string, bool) {
	chars, out := cStr(name), new(uintptr)
	if img.l.call("vips_image_get_as_string", img.handle, uintptr(unsafe.Pointer(&chars[0])), uintptr(unsafe.Pointer(out))) != 0 {
		img.l.call("vips_error_clear")
		return "", false
	}
	text := goStr(*out)
	img.l.call("g_free", *out)
	return text, true
}
