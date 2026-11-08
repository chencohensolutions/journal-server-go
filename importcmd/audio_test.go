package importcmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/journal/server-go/media"
)

func TestCallOf(t *testing.T) {
	local := func(y, mo, d, h, mi, s int) *time.Time {
		at := time.Date(y, time.Month(mo), d, h, mi, s, 0, time.Local)
		return &at
	}
	str := func(s string) *string { return &s }
	for _, c := range []struct {
		path            string
		contact, number *string
		started         *time.Time
	}{
		{"Call/Call recording אבא_250206_142813.m4a", str("אבא"), nil, local(2025, 2, 6, 14, 28, 13)},
		{"calls/הקלטת שיחה חבר_211004_170846.m4a", str("חבר"), nil, local(2021, 10, 4, 17, 8, 46)},
		{"Call/שיחה אחות_260627_214355.m4a", str("אחות"), nil, local(2026, 6, 27, 21, 43, 55)},
		{"Call recording 0501234567_250726_152307.m4a", nil, str("0501234567"), local(2025, 7, 26, 15, 23, 7)},
		{"Call recording +972501234567_250805_110244.m4a", nil, str("+972501234567"), local(2025, 8, 5, 11, 2, 44)},
		{"Call recording AT&T דני_250211_165104.m4a", str("AT&T דני"), nil, local(2025, 2, 11, 16, 51, 4)},
		{"Call recording 164_220307_125137.m4a", nil, str("164"), local(2022, 3, 7, 12, 51, 37)},
		{"Call/שיחה \u200f\u202a0521234567\u202c\u200f_261019_133143.m4a", nil, str("0521234567"), local(2026, 10, 19, 13, 31, 43)},
		{"Memento/Audio/Calls/2020-04-10 00.02.41.ogg", nil, nil, nil},
	} {
		call, started := callOf(filepath.FromSlash(c.path))
		switch {
		case call == nil:
			t.Errorf("%s: not a call", c.path)
		case !equal(call.Contact, c.contact) || !equal(call.Number, c.number):
			t.Errorf("%s: contact %v, number %v", c.path, deref(call.Contact), deref(call.Number))
		case (started == nil) != (c.started == nil) || started != nil && !started.Equal(*c.started):
			t.Errorf("%s: started %v, want %v", c.path, started, c.started)
		}
	}
	for _, path := range []string{"Recordings/recording-20220925-195923.flac", "Voice Notes/PTT-20250208-WA0004.opus", "Recordings/20250211 184146 14.aac"} {
		if call, _ := callOf(filepath.FromSlash(path)); call != nil {
			t.Errorf("%s: a call", path)
		}
	}
}

func TestTimeInRecordingNames(t *testing.T) {
	local := func(y, mo, d, h, mi, s int) time.Time {
		return time.Date(y, time.Month(mo), d, h, mi, s, 0, time.Local)
	}
	for name, want := range map[string]time.Time{
		// The part after ~ is not when it ended (one says ten days later of a 33-minute recording).
		"recording-20231008-182057~20231018-095751.ogg": local(2023, 10, 8, 18, 20, 57),
		"recording-20220925-195923.flac":                local(2022, 9, 25, 19, 59, 23),
		"20180929_134310-0.flac":                        local(2018, 9, 29, 13, 43, 10),
		"20250207 152917 12.aac":                        local(2025, 2, 7, 15, 29, 17),
		"20220119-050240.aac":                           local(2022, 1, 19, 5, 2, 40),
		"2020-04-10 00.02.41.ogg":                       local(2020, 4, 10, 0, 2, 41),
		"PTT-20250208-WA0004.opus":                      local(2025, 2, 8, 12, 0, 0),
	} {
		if got := timeInName(name); got == nil || !got.Equal(want) {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
	for _, name := range []string{"My recording 5.aac", "Call recording 0501234567_250726_152307.m4a"} {
		if got := timeInName(name); got != nil {
			t.Errorf("%s: %v, want none", name, got)
		}
	}
}

func TestAudioStart(t *testing.T) {
	plus2 := time.FixedZone("", 2*3600)
	length := 232 * time.Second
	// Samsung: the name is the local start, creation_time (UTC) the end, utc_offset the zone.
	named := time.Date(2025, 3, 17, 12, 15, 19, 0, time.Local)
	created := time.Date(2025, 3, 17, 10, 19, 11, 0, time.UTC)
	got, note := audioStart(&named, &created, length, plus2)
	if got == nil || got.Format(time.RFC3339) != "2025-03-17T12:15:19+02:00" || note != "" {
		t.Errorf("Samsung call: %v (%q)", got, note)
	}

	// Without a name: the creation time less the length.
	if got, _ := audioStart(nil, &created, length, plus2); got == nil || got.Format(time.RFC3339) != "2025-03-17T12:15:19+02:00" {
		t.Errorf("no name: %v", got)
	}
	// Without metadata: the name.
	if got, _ := audioStart(&named, nil, length, nil); got == nil || !got.Equal(named) {
		t.Errorf("no metadata: %v", got)
	}
	// Recorded at UTC+9 without saying so: the offset between the name and the start.
	abroad := time.Date(2024, 3, 1, 15, 0, 0, 0, time.Local)
	end := time.Date(2024, 3, 1, 6, 3, 52, 0, time.UTC)
	if got, _ := audioStart(&abroad, &end, length, nil); got == nil || got.Format("15:04:05") != "15:00:00" {
		t.Errorf("abroad: %v", got)
	} else if _, offset := got.Zone(); offset != 9*3600 {
		t.Errorf("abroad: offset %d", offset)
	}
	// A clock days off: the name, with a note.
	off := named.Add(72 * time.Hour).UTC()
	if got, note := audioStart(&named, &off, length, plus2); got == nil || !got.Equal(named) || note == "" {
		t.Errorf("clock off: %v (%q)", got, note)
	}
}

// TestWAVIsKeptAsFLAC imports a made-up WAV: the data folder gets a FLAC of the same length, which names the WAV
// it came from, and the WAV is found by it afterwards.
func TestWAVIsKeptAsFLAC(t *testing.T) {
	ffmpeg, ffprobe, err := media.FindFFmpeg("")
	if err != nil {
		t.Skip("needs ffmpeg")
	}
	dir := t.TempDir()
	wav := filepath.Join(dir, "in", "20210809_114634.wav")
	os.MkdirAll(filepath.Dir(wav), 0o755)
	if out, err := exec.Command(ffmpeg, "-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-c:a", "pcm_s16le", wav).CombinedOutput(); err != nil {
		t.Fatalf("making the WAV: %v: %s", err, out)
	}
	store, err := media.OpenStore(filepath.Join(dir, "data"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	im := &importer{store: store, ffmpeg: ffmpeg, ffprobe: ffprobe, notImportedRun: filepath.Join(dir, "not imported", "run"),
		counts: counts{audio: map[string]int{}}}
	info, _ := os.Stat(wav)
	sha, err := media.HashFile(wav)
	if err != nil {
		t.Fatal(err)
	}
	f := im.readFacts(context.Background(), wav)
	if f.kind != media.KindAudio || f.takenAt == nil || f.durationMs == nil || *f.durationMs < 900 {
		t.Fatalf("facts: %+v", f)
	}
	item, existed, err := im.importInto(context.Background(), wav, wav, sha, info.Size(), f, nil)
	if err != nil || existed {
		t.Fatalf("import: %v (existed %v)", err, existed)
	}
	facts := item.Sidecar.Facts
	if facts.MimeType != "audio/flac" || facts.FileName != "20210809_114634.flac" || filepath.Ext(item.Path) != ".flac" {
		t.Errorf("placed as %s (%s, %s)", item.Path, facts.FileName, facts.MimeType)
	}
	if e := facts.EncodedFrom; e == nil || e.Sha256 != sha || e.MimeType != "audio/wav" || e.Size != info.Size() {
		t.Errorf("encodedFrom %+v", facts.EncodedFrom)
	}
	if facts.Size >= info.Size() {
		t.Errorf("the FLAC has %d bytes, the WAV %d", facts.Size, info.Size())
	}
	if found, ok := store.EncodedFrom(sha); !ok || found.Sha256() != item.Sha256() {
		t.Errorf("the WAV isn't found by its FLAC")
	}
	if _, err := os.Stat(wav); err != nil {
		t.Errorf("the WAV is gone: %v", err)
	}
	if entries, _ := os.ReadDir(im.encodingDir()); len(entries) != 0 {
		t.Errorf("left in the encoding folder: %v", entries)
	}
}

func equal(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
