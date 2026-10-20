package main

import (
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestTimeInName(t *testing.T) {
	local := func(y, mo, d, h, mi, s int) time.Time {
		return time.Date(y, time.Month(mo), d, h, mi, s, 0, time.Local)
	}
	for name, want := range map[string]time.Time{
		"20251202_222720.mp4":        local(2025, 12, 2, 22, 27, 20),
		"20251201_090937(0).heic":    local(2025, 12, 1, 9, 9, 37),
		"VID_20190512_143022.mp4":    local(2019, 5, 12, 14, 30, 22),
		"IMG_20190512_143022123.jpg": local(2019, 5, 12, 14, 30, 22),
		"PXL_20230101_101010000.mp4": local(2023, 1, 1, 10, 10, 10),
		"2019-05-12 14.30.22.mov":    local(2019, 5, 12, 14, 30, 22),
		"VID-20190512-WA0001.mp4":    local(2019, 5, 12, 12, 0, 0),
	} {
		got := timeInName(name)
		if got == nil || !got.Equal(want) {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
	for _, name := range []string{"IMG_1234.jpg", "DSC00001.JPG", "20251399_222720.mp4", "video.mp4"} {
		if got := timeInName(name); got != nil {
			t.Errorf("%s: %v, want none", name, got)
		}
	}
}

func TestWithOffsetFromName(t *testing.T) {
	ms := int64(30_000)
	// Filmed abroad at UTC+9: the name's 15:00 is 06:00 UTC, the creation time is when filming stopped.
	named := time.Date(2024, 3, 1, 15, 0, 0, 0, time.Local)
	created := time.Date(2024, 3, 1, 6, 0, 31, 0, time.UTC)
	got := withOffsetFromName(created, named, &ms)
	if _, offset := got.Zone(); offset != 9*3600 {
		t.Errorf("offset %d, want 9h", offset)
	}
	if !got.Equal(created) {
		t.Errorf("instant %v, want %v", got, created)
	}
	if got.Format("15:04") != "15:00" {
		t.Errorf("local time %s, want 15:00", got.Format("15:04"))
	}

	// Filmed here: this machine's time zone.
	here := time.Date(2024, 3, 1, 15, 0, 0, 0, time.Local)
	if got := withOffsetFromName(here.Add(31*time.Second).UTC(), here, &ms); got.Location() != time.Local {
		t.Errorf("zone %v, want local", got.Location())
	}

	// A camera clock days off: the name wins.
	if got := withOffsetFromName(named.Add(72*time.Hour), named, &ms); !got.Equal(named) {
		t.Errorf("%v, want the name's %v", got, named)
	}
}

func TestExifTime(t *testing.T) {
	got := exifTime("2025:12:04 14:28:36", "+02:00")
	if got == nil || got.Format(time.RFC3339) != "2025-12-04T14:28:36+02:00" {
		t.Errorf("with offset: %v", got)
	}
	got = exifTime("2025:12:04 14:28:36", "")
	if got == nil || got.Location() != time.Local {
		t.Errorf("without offset: %v", got)
	}
	for _, empty := range []string{"", "0000:00:00 00:00:00", "1970:01:01 00:00:00", "    :  :     :  :  "} {
		if got := exifTime(empty, ""); got != nil {
			t.Errorf("%q: %v", empty, got)
		}
	}
}

func TestGPSDegrees(t *testing.T) {
	lat, ok := gpsDegrees("32/1 50/1 49947000/1000000", "N", "S")
	if !ok || lat < 32.8472 || lat > 32.8473 {
		t.Errorf("lat %v %v", lat, ok)
	}
	lon, ok := gpsDegrees("73/1 59/1 0/1", "W", "W")
	if !ok || lon > -73.98 || lon < -73.99 {
		t.Errorf("lon %v %v", lon, ok)
	}
	if _, ok := gpsDegrees("", "", "S"); ok {
		t.Error("empty parsed")
	}
	if p := parseISO6709("+32.0853+034.7818/"); p == nil || p.Lat != 32.0853 || p.Lon != 34.7818 {
		t.Errorf("ISO 6709: %+v", p)
	}
}

func TestFoldersAndPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		roots := []string{`D:\Archive\Hen\phone DCIM`, `E:\`}
		if got := pathInside(roots, `D:\Archive\Hen\phone DCIM\Camera\x.jpg`); got != filepath.Join("phone DCIM", "Camera", "x.jpg") {
			t.Errorf("pathInside %s", got)
		}
		if got := pathInside(roots, `E:\DCIM\x.jpg`); got != filepath.Join("E", "DCIM", "x.jpg") {
			t.Errorf("pathInside of a drive %s", got)
		}
		if !within(`D:\Archive\Hen\journal\media`, `d:\archive\hen\JOURNAL`) {
			t.Error("within ignores case")
		}
	}
	dir := filepath.Join("a", "data")
	if !within(dir, dir) || !within(filepath.Join(dir, "x"), dir) || within(filepath.Join("a", "database"), dir) || within("a", dir) {
		t.Error("within")
	}
	for path, want := range map[string]string{
		"/b/Phone/DCIM/Camera/x.jpg":             "camera",
		"/b/Phone/DCIM/Screenshots/x.png":        "screenshots",
		"/b/WhatsApp/Media/WhatsApp Video/x.mp4": "whatsapp",
		"/b/Pictures/x.jpg":                      "other",
		"/Hen/phone DCIM/Camera/x.jpg":           "camera",
	} {
		if got := sourceOfPath(filepath.FromSlash(path)); got != want {
			t.Errorf("%s: %s, want %s", path, got, want)
		}
	}
}

func TestNameFirst(t *testing.T) {
	named := time.Date(2025, 12, 4, 14, 28, 35, 0, time.Local)
	exif := time.Date(2025, 12, 4, 14, 28, 36, 0, time.FixedZone("", 2*3600))
	if got, note := nameFirst(&named, &exif, 2*time.Minute); got != &exif || note != "" {
		t.Errorf("agreeing: %v %q, want the metadata's (it has the offset)", got, note)
	}
	wrong := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)
	if got, note := nameFirst(&named, &wrong, 2*time.Minute); got != &named || note == "" {
		t.Errorf("disagreeing: %v %q, want the name's and a note", got, note)
	}
	if got, _ := nameFirst(nil, &exif, time.Minute); got != &exif {
		t.Errorf("no name: %v", got)
	}
}

func TestReadPNM(t *testing.T) {
	data := []byte("P6\n#vips2ppm - Tue Oct 20 2026\n2 1\n255\n")
	data = append(data, 255, 255, 255, 0, 0, 0)
	grey, err := readPNM(data, 2, 1)
	if err != nil || len(grey) != 2 || grey[0] != 255*1000 || grey[1] != 0 {
		t.Fatalf("%v %v", grey, err)
	}
	if _, err := readPNM(data, 9, 8); err == nil {
		t.Error("wrong size accepted")
	}
	if d := dhash(0b1011).distance(0b0010); d != 2 {
		t.Errorf("distance %d", d)
	}
}

func TestSameBurst(t *testing.T) {
	at := func(s string) *time.Time {
		v, _ := time.Parse("2006-01-02 15:04:05.000", s)
		return &v
	}
	shot := func(name, taken string) photo { return photo{name: name, takenAt: at(taken)} }
	for _, c := range []struct {
		a, b  photo
		burst bool
	}{
		{shot("20260120_081253_035.jpg", "2026-01-20 08:12:53.000"), shot("20260120_081253_041.jpg", "2026-01-20 08:12:53.000"), true},
		{shot("PIC_20251202_192632357.jpg", "2025-12-02 19:26:32.000"), shot("PIC_20251202_192632912.jpg", "2025-12-02 19:26:32.000"), true},
		{shot("20260505_181704.jpg", "2026-05-05 18:17:04.000"), shot("20260505_181709.jpg", "2026-05-05 18:17:09.000"), true},
		// The same shot twice (a resized or sent copy): for the owner to decide.
		{shot("20260505_181704.jpg", "2026-05-05 18:17:04.000"), shot("20260505_181704 (2).jpg", "2026-05-05 18:17:04.000"), false},
		{shot("20260505_181704.jpg", "2026-05-05 18:17:04.000"), shot("IMG-20260506-WA0003.jpg", "2026-05-06 12:00:00.000"), false},
		{shot("a.jpg", "2026-05-05 18:17:04.000"), photo{name: "b.jpg"}, false},
	} {
		if got := sameBurst(c.a, c.b); got != c.burst {
			t.Errorf("%s / %s: burst %v, want %v", c.a.name, c.b.name, got, c.burst)
		}
	}
}

func TestFolderOf(t *testing.T) {
	root := filepath.Join("D", "Archive", "phone DCIM")
	if got := folderOf(root, filepath.Join(root, "Flowers", "x.jpg")); got != "phone DCIM/Flowers" {
		t.Errorf("%q", got)
	}
	if got := folderOf(root, filepath.Join(root, "2026-06", "2025", "x.jpg")); got != "phone DCIM/2026-06/2025" {
		t.Errorf("%q", got)
	}
	if got := folderOf(root, filepath.Join(root, "x.jpg")); got != "phone DCIM" {
		t.Errorf("%q", got)
	}
}
