package main

import (
	"context"
	"path/filepath"
	"time"

	"github.com/journal/server-go/media"
)

// Sound recordings: recordings of events, recorded phone calls and WhatsApp voice messages. What their files say
// (dates, length, format, who a call was with) is read by package media (audionames.go), shared with uploads from
// devices; here it is read in this machine's time zone. A WAV is kept as FLAC (encodeFLAC): the same sound in about
// half the bytes.

// callOf is who a call was with and when it started, from the file's name and folder (see media.CallFromName).
func callOf(path string) (call *media.Call, started *time.Time) {
	return media.CallFromName(filepath.Base(path), filepath.Base(filepath.Dir(path)), time.Local)
}

// audioFacts reads the recording's length, start and format with ffprobe, and who a call was with from its name.
// A file ffprobe can't read (a call whose recording never finished has no moov) fails.
func (im *importer) audioFacts(ctx context.Context, path string, f *facts) {
	a := media.ProbeAudio(ctx, im.ffprobe, path, filepath.Base(path), filepath.Base(filepath.Dir(path)), nil)
	if a.Failed != "" {
		f.failed = a.Failed
		return
	}
	f.durationMs = a.DurationMs
	if a.MimeType != "" {
		f.mime = a.MimeType
	}
	f.call = a.Call
	f.takenAt, f.note = a.TakenAt, a.Note
}

// audioStart is media.AudioStart in this machine's time zone.
func audioStart(named, created *time.Time, length time.Duration, zone *time.Location) (*time.Time, string) {
	return media.AudioStart(named, created, length, zone, time.Local)
}

// encodeFLAC encodes a WAV as FLAC in work, keeping its metadata, and returns the FLAC's path.
func encodeFLAC(ctx context.Context, ffmpeg, work, wav string) (string, error) {
	return media.EncodeFLAC(ctx, ffmpeg, work, wav)
}

// isWAV reports whether the import keeps the file as FLAC.
func isWAV(path string) bool { return media.IsWAV(path, "") }
