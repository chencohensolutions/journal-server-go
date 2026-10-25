package media

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// What a sound recording's file says about it: when it started, how long it is, its format, and for a phone call who
// it was with. Shared by scripts/import and device uploads (Store.finish), so a recording gets the same facts however
// it arrives. Names are read in the time zone of where the recording was made when that is known (a device's
// takenAt), else this machine's.

// Samsung's call recorder names a call after the contact or, without one, the number, then the local start
// with a two-digit year: "Call recording אבא_250206_142813", "הקלטת שיחה 0501234567_211115_200618", and since
// 2026 "שיחה אבא_260724_204448".
var samsungCall = regexp.MustCompile(`^(?:Call recording|הקלטת שיחה|שיחה) (.+)_(\d{6})_(\d{6})$`)

// A phone number as recorders write it: 0501234567, +972501234567, *3123, 164.
var phoneNumber = regexp.MustCompile(`^\+?[\d*#]{3,15}$`)

// CallFromName is who a call was with and when it started, from its file name (without the invisible marks of writing
// direction Samsung puts around a number in a Hebrew name: U+200F, U+202A, U+202C), the start read in loc. With a
// name that says neither, a file in a folder named Call or Calls (Memento's Audio/Calls; [folder] is that folder's
// name) is a call with no one named. Nil for anything else.
func CallFromName(fileName, folder string, loc *time.Location) (call *Call, started *time.Time) {
	base := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	base = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, base)
	if m := samsungCall.FindStringSubmatch(base); m != nil {
		who := strings.TrimSpace(m[1])
		call = &Call{}
		if phoneNumber.MatchString(who) {
			call.Number = &who
		} else {
			call.Contact = &who
		}
		if t, err := time.ParseInLocation("060102 150405", m[2]+" "+m[3], loc); err == nil && Plausible(t) {
			started = &t
		}
		return call, started
	}
	if dir := strings.ToLower(folder); dir == "call" || dir == "calls" {
		return &Call{}, nil
	}
	return nil, nil
}

// Dates in file names: 20190512_143022, IMG_20190512_143022123, VID-20190512-WA0001 (no time),
// 2019-05-12 14.30.22. DateTimeInName is the one with a time; the import's same-picture check reads it too.
var (
	DateTimeInName = regexp.MustCompile(`(?:^|\D)((?:19|20)\d\d)[-_.]?(\d\d)[-_.]?(\d\d)[ _\-T.]?(\d\d)[-_.:]?(\d\d)[-_.:]?(\d\d)`)
	dateInName     = regexp.MustCompile(`(?:^|\D)((?:19|20)\d\d)-?(\d\d)-?(\d\d)(?:\D|$)`)
)

// TimeInName reads a date in the file name, in loc. A date without a time is taken as noon, so it stays on its day.
func TimeInName(name string, loc *time.Location) *time.Time {
	if m := DateTimeInName.FindStringSubmatch(name); m != nil {
		t, err := time.ParseInLocation("2006 01 02 15 04 05", strings.Join(m[1:], " "), loc)
		if err == nil && Plausible(t) {
			return &t
		}
	}
	if m := dateInName.FindStringSubmatch(name); m != nil {
		t, err := time.ParseInLocation("2006 01 02 15", strings.Join(m[1:], " ")+" 12", loc)
		if err == nil && Plausible(t) {
			return &t
		}
	}
	return nil
}

// Plausible rules out empty dates (QuickTime's 1904, Unix's 1970) and dates in the future.
func Plausible(t time.Time) bool {
	return t.Year() >= 1990 && t.Before(time.Now().Add(48*time.Hour))
}

// WallClock is t's local date and time, as if it were UTC, to compare times written in different zones.
func WallClock(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
}

// WithOffsetFromName returns a UTC creation time in the time zone it was made in: loc when the local time in the
// name matches (the name is when it started, the creation time may be when it stopped), else the offset between the
// two, to the quarter hour. When they are too far apart for any time zone, the clock was off and the name is used.
func WithOffsetFromName(created, named time.Time, durationMs *int64, loc *time.Location) *time.Time {
	var length time.Duration
	if durationMs != nil {
		length = time.Duration(*durationMs) * time.Millisecond
	}
	if gap := created.Sub(named); gap >= -2*time.Minute && gap <= length+2*time.Minute {
		local := created.In(loc)
		return &local
	}
	wall := time.Date(named.Year(), named.Month(), named.Day(), named.Hour(), named.Minute(), named.Second(), 0, time.UTC)
	offset := wall.Sub(created).Round(15 * time.Minute)
	if offset < -12*time.Hour || offset > 14*time.Hour {
		return &named
	}
	t := created.In(time.FixedZone("", int(offset.Seconds())))
	return &t
}

// AudioStart is when a recording started. With a name, the name's local time, with the offset of the creation time
// (in zone, else loc) when that is the start or the end and shows the same local time, else the offset between the
// two. Without a name, the creation time less the length. note says when the name and the metadata are more than a
// day apart.
func AudioStart(named, created *time.Time, length time.Duration, zone, loc *time.Location) (*time.Time, string) {
	if created == nil {
		return named, ""
	}
	in := func(t time.Time) time.Time {
		if zone != nil {
			return t.In(zone)
		}
		return t.In(loc)
	}
	start := in(created.Add(-length))
	if named == nil {
		return &start, ""
	}
	for _, at := range []time.Time{start, in(*created)} {
		if WallClock(at).Sub(WallClock(*named)).Abs() <= 2*time.Minute {
			return &at, ""
		}
	}
	// Recorded in another time zone than loc, without saying which.
	if zone == nil {
		if t := WithOffsetFromName(created.Add(-length), *named, nil, loc); t != named {
			return t, ""
		}
	}
	note := ""
	if WallClock(start).Sub(WallClock(*named)).Abs() > 24*time.Hour {
		note = fmt.Sprintf("name %s, metadata %s", named.Format("2006-01-02 15:04:05"), start.Format(time.RFC3339))
	}
	return named, note
}

// audioMime is the MIME type of each format ffprobe names.
var audioMime = map[string]string{
	"mov,mp4,m4a,3gp,3g2,mj2": "audio/mp4",
	"aac":                     "audio/aac",
	"flac":                    "audio/flac",
	"ogg":                     "audio/ogg",
	"wav":                     "audio/wav",
}

// AudioFacts is what ProbeAudio read of a recording. Failed says why ffprobe couldn't read it as sound; the rest is
// then empty but for Call and TakenAt, which come from the name.
type AudioFacts struct {
	DurationMs *int64
	MimeType   string
	TakenAt    *time.Time
	Call       *Call
	Note       string
	Failed     string
}

// ProbeAudio reads a recording's length, start and format with ffprobe, and who a call was with from its name
// ([fileName], in the folder named [folder]). Names are read in the zone of [hint] (the time a device sent), else this
// machine's. A file ffprobe can't read (a call whose recording never finished has no moov) has Failed set.
func ProbeAudio(ctx context.Context, ffprobe, path, fileName, folder string, hint *time.Time) AudioFacts {
	loc := time.Local
	if hint != nil {
		loc = hint.Location()
	}
	var f AudioFacts
	call, named := CallFromName(fileName, folder, loc)
	f.Call = call
	if named == nil {
		named = TimeInName(fileName, loc)
	}
	var probe struct {
		Format struct {
			FormatName string            `json:"format_name"`
			Duration   string            `json:"duration"`
			Tags       map[string]string `json:"tags"`
		} `json:"format"`
	}
	var err error
	var stderr strings.Builder
	if ffprobe == "" {
		err = fmt.Errorf("ffprobe is not installed")
	} else {
		cmd := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_entries", "format=format_name,duration:format_tags", "-of", "json", path)
		cmd.Stderr = &stderr
		var out []byte
		if out, err = cmd.Output(); err == nil {
			json.Unmarshal(out, &probe)
		}
	}
	seconds, errLength := strconv.ParseFloat(probe.Format.Duration, 64)
	if err != nil || errLength != nil || seconds <= 0 {
		why, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		if why == "" && err != nil {
			why = err.Error()
		}
		f.Failed = "ffprobe can't read it as sound: " + why
		f.TakenAt = named
		return f
	}
	ms := int64(seconds * 1000)
	f.DurationMs = &ms
	// The format inside, not the name's: some recorders write MP4 into .aac files.
	f.MimeType = audioMime[probe.Format.FormatName]
	var created *time.Time
	if t, err := time.Parse(time.RFC3339Nano, probe.Format.Tags["creation_time"]); err == nil && Plausible(t) {
		created = &t
	}
	// Samsung writes the offset of where it recorded: "+0200".
	var zone *time.Location
	if o, err := time.Parse("-0700", probe.Format.Tags["com.samsung.android.utc_offset"]); err == nil {
		zone = o.Location()
	}
	f.TakenAt, f.Note = AudioStart(named, created, time.Duration(ms)*time.Millisecond, zone, loc)
	return f
}

// EncodeFLAC encodes a WAV as FLAC in work, keeping its metadata, and returns the FLAC's path.
func EncodeFLAC(ctx context.Context, ffmpeg, work, wav string) (string, error) {
	if ffmpeg == "" {
		return "", ErrNoEncoder
	}
	out := filepath.Join(work, strings.TrimSuffix(filepath.Base(wav), filepath.Ext(wav))+".flac")
	cmd := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-i", wav,
		"-map_metadata", "0", "-map", "0:a", "-c:a", "flac", "-compression_level", "8", out)
	if msg, err := cmd.CombinedOutput(); err != nil {
		os.Remove(out)
		return "", fmt.Errorf("encoding as FLAC: %v: %s", err, strings.TrimSpace(string(msg)))
	}
	return out, nil
}

// ErrNoEncoder is a WAV that can't be kept as FLAC: ffmpeg isn't installed.
var ErrNoEncoder = fmt.Errorf("ffmpeg is needed to keep a WAV as FLAC")

// IsWAV reports whether a file is kept as FLAC (by its name or MIME type).
func IsWAV(name, mimeType string) bool {
	m := strings.ToLower(mimeType)
	return strings.EqualFold(filepath.Ext(name), ".wav") || m == "audio/wav" || m == "audio/x-wav" || m == "audio/wave"
}
