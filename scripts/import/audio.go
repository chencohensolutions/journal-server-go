package main

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

	"github.com/journal/server-go/media"
)

// Sound recordings: recordings of events, recorded phone calls and WhatsApp voice messages. Their date is the
// file name's (recorders name files by when they started), given its offset by the metadata's creation_time,
// which Android writes when the recording stops, so it is the end. A WAV is kept as FLAC (encodeFLAC): the same
// sound in about half the bytes.

// Samsung's call recorder names a call after the contact or, without one, the number, then the local start
// with a two-digit year: "Call recording אבא_250206_142813", "הקלטת שיחה 0501234567_211115_200618", and since
// 2026 "שיחה אבא_260724_204448".
var samsungCall = regexp.MustCompile(`^(?:Call recording|הקלטת שיחה|שיחה) (.+)_(\d{6})_(\d{6})$`)

// A phone number as recorders write it: 0501234567, +972501234567, *3123, 164.
var phoneNumber = regexp.MustCompile(`^\+?[\d*#]{3,15}$`)

// callOf is who a call was with and when it started, from its file name, without the invisible marks of writing
// direction Samsung puts around a number in a Hebrew name (U+200F, U+202A, U+202C). With a name that says neither, a file
// in a folder named Call or Calls (Memento's Audio/Calls) is a call with no one named. Nil for anything else.
func callOf(path string) (call *media.Call, started *time.Time) {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	base = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, base)
	if m := samsungCall.FindStringSubmatch(base); m != nil {
		who := strings.TrimSpace(m[1])
		call = &media.Call{}
		if phoneNumber.MatchString(who) {
			call.Number = &who
		} else {
			call.Contact = &who
		}
		if t, err := time.ParseInLocation("060102 150405", m[2]+" "+m[3], time.Local); err == nil && plausible(t) {
			started = &t
		}
		return call, started
	}
	if dir := strings.ToLower(filepath.Base(filepath.Dir(path))); dir == "call" || dir == "calls" {
		return &media.Call{}, nil
	}
	return nil, nil
}

// audioFacts reads the recording's length, start and format with ffprobe, and who a call was with from its name.
// A file ffprobe can't read (a call whose recording never finished has no moov) fails.
func (im *importer) audioFacts(ctx context.Context, path string, f *facts) {
	var probe struct {
		Format struct {
			FormatName string            `json:"format_name"`
			Duration   string            `json:"duration"`
			Tags       map[string]string `json:"tags"`
		} `json:"format"`
	}
	cmd := exec.CommandContext(ctx, im.ffprobe, "-v", "error", "-show_entries", "format=format_name,duration:format_tags",
		"-of", "json", path)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		json.Unmarshal(out, &probe)
	}
	seconds, errLength := strconv.ParseFloat(probe.Format.Duration, 64)
	if err != nil || errLength != nil || seconds <= 0 {
		why, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		f.failed = "ffprobe can't read it as sound: " + why
		return
	}
	ms := int64(seconds * 1000)
	f.durationMs = &ms
	length := time.Duration(ms) * time.Millisecond
	// The format inside, not the name's: some recorders write MP4 into .aac files.
	if mime := audioMime[probe.Format.FormatName]; mime != "" {
		f.mime = mime
	}

	call, named := callOf(path)
	f.call = call
	if named == nil {
		named = timeInName(filepath.Base(path))
	}
	var created *time.Time
	if t, err := time.Parse(time.RFC3339Nano, probe.Format.Tags["creation_time"]); err == nil && plausible(t) {
		created = &t
	}
	// Samsung writes the offset of where it recorded: "+0200".
	var zone *time.Location
	if o, err := time.Parse("-0700", probe.Format.Tags["com.samsung.android.utc_offset"]); err == nil {
		zone = o.Location()
	}
	f.takenAt, f.note = audioStart(named, created, length, zone)
}

// audioStart is when a recording started. With a name, the name's local time, with the offset of the creation time
// (in zone, else this machine's time zone) when that is the start or the end and shows the same local time, else
// the offset between the two. Without a name, the creation time less the length. note says when the name and the
// metadata are more than a day apart.
func audioStart(named, created *time.Time, length time.Duration, zone *time.Location) (*time.Time, string) {
	if created == nil {
		return named, ""
	}
	in := func(t time.Time) time.Time {
		if zone != nil {
			return t.In(zone)
		}
		return t.In(time.Local)
	}
	start := in(created.Add(-length))
	if named == nil {
		return &start, ""
	}
	for _, at := range []time.Time{start, in(*created)} {
		if wallClock(at).Sub(wallClock(*named)).Abs() <= 2*time.Minute {
			return &at, ""
		}
	}
	// Recorded in another time zone than this machine's, without saying which.
	if zone == nil {
		if t := withOffsetFromName(created.Add(-length), *named, nil); t != named {
			return t, ""
		}
	}
	note := ""
	if wallClock(start).Sub(wallClock(*named)).Abs() > 24*time.Hour {
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

// encodeFLAC encodes a WAV as FLAC in work, keeping its metadata, and returns the FLAC's path.
func encodeFLAC(ctx context.Context, ffmpeg, work, wav string) (string, error) {
	if ffmpeg == "" {
		return "", fmt.Errorf("ffmpeg is needed to keep a WAV as FLAC")
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

// isWAV reports whether the import keeps the file as FLAC.
func isWAV(path string) bool { return strings.EqualFold(filepath.Ext(path), ".wav") }
