package importcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/journal/server-go/media"
)

// Reading what a device would have sent with the upload: the date, size, length and place of a photo or video.

// facts are what a device would have sent with the upload.
type facts struct {
	kind       media.Kind
	mime       string
	takenAt    *time.Time
	width      *int
	height     *int
	durationMs *int64
	location   *media.GeoPoint
	call       *media.Call       // a recorded phone call
	camera     *media.Camera     // the camera that took it, from its EXIF or its video's tags
	failed     string            // why the file can't be imported (a sound recording ffprobe can't read)
	note       string            // the name's and the metadata's dates disagree
	exif       map[string]string // a photo's fields as vipsheader names them, shown by --review
}

func (im *importer) readFacts(ctx context.Context, path string) facts {
	t := mediaTypes[strings.ToLower(filepath.Ext(path))]
	f := facts{kind: t.kind, mime: t.mime}
	switch t.kind {
	case media.KindVideo:
		im.videoFacts(ctx, path, &f)
	case media.KindAudio:
		im.audioFacts(ctx, path, &f)
	default:
		im.photoFacts(ctx, path, &f)
	}
	return f
}

// photoFacts reads the photo's size, GPS position and EXIF date with vipsheader (a DNG's EXIF from the
// file itself, as vipsheader doesn't show it).
func (im *importer) photoFacts(ctx context.Context, path string, f *facts) {
	fields := map[string]string{}
	if out, err := exec.CommandContext(ctx, im.vipsheader, "-a", path).Output(); err == nil {
		for line := range strings.SplitSeq(string(out), "\n") {
			name, value, ok := strings.Cut(strings.TrimRight(line, "\r"), ": ")
			if !ok {
				continue
			}
			// EXIF fields read "2019:05:12 14:30:22 (2019:05:12 14:30:22, ASCII, 20 components, 20 bytes)".
			raw, _, _ := strings.Cut(value, " (")
			fields[name] = strings.TrimSpace(raw)
		}
	}
	if strings.EqualFold(filepath.Ext(path), ".dng") {
		if exif, err := media.TiffExif(path); err == nil {
			maps.Copy(fields, exif)
		}
	}

	f.exif = fields
	f.camera = media.CameraFromEXIF(fields)
	exif := exifTime(fields["exif-ifd2-DateTimeOriginal"], fields["exif-ifd2-OffsetTimeOriginal"])
	if exif == nil {
		exif = exifTime(fields["exif-ifd2-DateTimeDigitized"], fields["exif-ifd2-OffsetTimeDigitized"])
	}
	if exif == nil {
		// Editing programs also change this one.
		exif = exifTime(fields["exif-ifd0-DateTime"], fields["exif-ifd2-OffsetTime"])
	}
	f.takenAt, f.note = nameFirst(timeInName(filepath.Base(path)), exif, 2*time.Minute)

	width, errW := strconv.Atoi(fields["width"])
	height, errH := strconv.Atoi(fields["height"])
	if errW == nil && errH == nil && width > 0 && height > 0 {
		// Upright, as devices send it: a photo turned a quarter (orientation 5–8) is shown the other way round.
		if o, _ := strconv.Atoi(fields["orientation"]); o >= 5 && o <= 8 {
			width, height = height, width
		}
		f.width, f.height = &width, &height
	}

	lat, okLat := gpsDegrees(fields["exif-ifd3-GPSLatitude"], fields["exif-ifd3-GPSLatitudeRef"], "S")
	lon, okLon := gpsDegrees(fields["exif-ifd3-GPSLongitude"], fields["exif-ifd3-GPSLongitudeRef"], "W")
	if okLat && okLon && (lat != 0 || lon != 0) && lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180 {
		f.location = &media.GeoPoint{Lat: lat, Lon: lon}
	}
}

// nameFirst picks the date: the file name's, or the metadata's when they show the same local time
// (within slack), since the metadata also has the offset. Without a date in the name, the metadata's.
// note says when the two are more than a day apart.
func nameFirst(named, metadata *time.Time, slack time.Duration) (*time.Time, string) {
	switch {
	case named == nil:
		return metadata, ""
	case metadata == nil:
		return named, ""
	}
	gap := wallClock(*metadata).Sub(wallClock(*named)).Abs()
	if gap <= slack {
		return metadata, ""
	}
	if gap > 24*time.Hour {
		return named, fmt.Sprintf("name %s, metadata %s", named.Format("2006-01-02 15:04:05"), metadata.Format(time.RFC3339))
	}
	return named, ""
}

// wallClock is media.WallClock.
func wallClock(t time.Time) time.Time { return media.WallClock(t) }

// videoFacts reads the video's size, length and date with ffprobe. Phones write the date in UTC
// (creation_time) and the local time in the file name, so the two together give the offset; Apple's
// creationdate already has one.
func (im *importer) videoFacts(ctx context.Context, path string, f *facts) {
	var probe struct {
		Streams []struct {
			CodecType    string            `json:"codec_type"`
			Width        int               `json:"width"`
			Height       int               `json:"height"`
			Tags         map[string]string `json:"tags"`
			SideDataList []struct {
				Rotation float64 `json:"rotation"`
			} `json:"side_data_list"`
		} `json:"streams"`
		Format struct {
			Duration string            `json:"duration"`
			Tags     map[string]string `json:"tags"`
		} `json:"format"`
	}
	out, err := exec.CommandContext(ctx, im.ffprobe, "-v", "error", "-show_entries",
		"format=duration:format_tags:stream=codec_type,width,height:stream_tags=rotate:stream_side_data=rotation",
		"-of", "json", path).Output()
	if err == nil {
		json.Unmarshal(out, &probe)
	}

	for _, s := range probe.Streams {
		if s.CodecType != "video" || s.Width <= 0 || s.Height <= 0 {
			continue
		}
		width, height := s.Width, s.Height
		rotation, _ := strconv.ParseFloat(s.Tags["rotate"], 64)
		for _, side := range s.SideDataList {
			if side.Rotation != 0 {
				rotation = side.Rotation
			}
		}
		if r := int(rotation) % 180; r == 90 || r == -90 {
			width, height = height, width
		}
		f.width, f.height = &width, &height
		break
	}
	var length time.Duration
	if seconds, err := strconv.ParseFloat(probe.Format.Duration, 64); err == nil && seconds > 0 {
		ms := int64(seconds * 1000)
		f.durationMs = &ms
		length = time.Duration(ms) * time.Millisecond
	}
	f.camera = media.CameraFromTags(probe.Format.Tags)
	if iso6709 := probe.Format.Tags["location"]; iso6709 != "" {
		f.location = parseISO6709(iso6709)
	}

	named := timeInName(filepath.Base(path))
	var apple, created *time.Time
	if value := probe.Format.Tags["com.apple.quicktime.creationdate"]; value != "" {
		for _, layout := range []string{"2006-01-02T15:04:05-0700", time.RFC3339} {
			if t, err := time.Parse(layout, value); err == nil && plausible(t) {
				apple = &t
				break
			}
		}
	}
	if t, err := time.Parse(time.RFC3339Nano, probe.Format.Tags["creation_time"]); err == nil && plausible(t) {
		created = &t
	}
	switch {
	case apple != nil:
		f.takenAt, f.note = nameFirst(named, apple, length+2*time.Minute)
	case named != nil && created != nil:
		f.takenAt = withOffsetFromName(*created, *named, f.durationMs)
		if gap := created.Sub(*named).Abs(); gap > 38*time.Hour { // further than any two time zones
			f.note = fmt.Sprintf("name %s, metadata %s", named.Format("2006-01-02 15:04:05"), created.Format(time.RFC3339))
		}
	case named != nil:
		f.takenAt = named
	case created != nil:
		local := created.In(time.Local)
		f.takenAt = &local
	}
}

// withOffsetFromName is media.WithOffsetFromName in this machine's time zone: a video's UTC creation time in the
// zone it was filmed in.
func withOffsetFromName(created, named time.Time, durationMs *int64) *time.Time {
	return media.WithOffsetFromName(created, named, durationMs, time.Local)
}

// exifTime parses an EXIF date ("2019:05:12 14:30:22") with its offset ("+03:00"), or in this machine's
// time zone without one.
func exifTime(value, offset string) *time.Time {
	if value == "" || strings.HasPrefix(value, "0000") {
		return nil
	}
	location := time.Local
	if offset != "" {
		if o, err := time.Parse("-07:00", offset); err == nil {
			location = o.Location()
		}
	}
	t, err := time.ParseInLocation("2006:01:02 15:04:05", value, location)
	if err != nil || !plausible(t) {
		return nil
	}
	return &t
}

// dateTimeInName finds a date and time in a file name (media.DateTimeInName).
var dateTimeInName = media.DateTimeInName

// timeInName reads a date in the file name, in this machine's time zone (media.TimeInName).
func timeInName(name string) *time.Time { return media.TimeInName(name, time.Local) }

// plausible rules out empty dates and dates in the future (media.Plausible).
func plausible(t time.Time) bool { return media.Plausible(t) }

// gpsDegrees parses EXIF's degrees, minutes and seconds ("32/1 50/1 49947000/1000000"), negative when
// ref is negativeRef ("S" or "W").
func gpsDegrees(value, ref, negativeRef string) (float64, bool) {
	parts := strings.Fields(value)
	if len(parts) != 3 {
		return 0, false
	}
	var degrees float64
	for i, part := range parts {
		num, den, ok := strings.Cut(part, "/")
		n, errN := strconv.ParseFloat(num, 64)
		d, errD := strconv.ParseFloat(den, 64)
		if !ok || errN != nil || errD != nil || d == 0 {
			return 0, false
		}
		degrees += n / d / []float64{1, 60, 3600}[i]
	}
	if strings.EqualFold(ref, negativeRef) {
		degrees = -degrees
	}
	return degrees, true
}

// iso6709Point matches a video's location tag, "+32.0853+034.7818/" or "+32.0853+034.7818+012.000/".
var iso6709Point = regexp.MustCompile(`^([+-]\d+(?:\.\d+)?)([+-]\d+(?:\.\d+)?)`)

func parseISO6709(value string) *media.GeoPoint {
	m := iso6709Point.FindStringSubmatch(value)
	if m == nil {
		return nil
	}
	lat, errLat := strconv.ParseFloat(m[1], 64)
	lon, errLon := strconv.ParseFloat(m[2], 64)
	if errLat != nil || errLon != nil || (lat == 0 && lon == 0) || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return nil
	}
	return &media.GeoPoint{Lat: lat, Lon: lon}
}
