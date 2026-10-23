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

func TestShotTime(t *testing.T) {
	exif := func(sub string) map[string]string {
		return map[string]string{"exif-ifd2-DateTimeOriginal": "2026:01:20 08:12:53", "exif-ifd2-SubSecTimeOriginal": sub}
	}
	for _, c := range []struct {
		exif  map[string]string
		name  string
		want  string
		exact bool
	}{
		{exif("035"), "a.jpg", "08:12:53.035", true},
		{exif("35"), "a.jpg", "08:12:53.350", true}, // sub-seconds are digits after the point
		{exif(""), "20260120_081253_041.jpg", "08:12:53.041", true},
		{exif(""), "PIC_20260120_081253912.jpg", "08:12:53.912", true},
		{exif(""), "20260120_081254_041.jpg", "08:12:53.000", false}, // the name's second differs
		{exif(""), "IMG-20260120-WA0003.jpg", "08:12:53.000", false},
	} {
		got, exact := shotTime(c.exif, c.name)
		if got == nil || got.Format("15:04:05.000") != c.want || exact != c.exact {
			t.Errorf("%v %s: %v %v, want %s %v", c.exif, c.name, got, exact, c.want, c.exact)
		}
	}
	if got, _ := shotTime(map[string]string{}, "20260120_081253_041.jpg"); got != nil {
		t.Errorf("no EXIF date: %v", got)
	}
}

func TestDifferentShots(t *testing.T) {
	at := func(s string) *time.Time {
		v, _ := time.Parse("15:04:05.000", s)
		return &v
	}
	shot := func(camera, taken string, exact bool) *picture {
		return &picture{camera: camera, shot: at(taken), exact: exact}
	}
	for _, c := range []struct {
		a, b      *picture
		different bool
	}{
		{shot("samsung SM-S918B", "08:12:53.035", true), shot("samsung SM-S918B", "08:12:53.041", true), false},
		{shot("samsung SM-S918B", "08:12:53.035", true), shot("samsung SM-S918B", "08:12:53.050", true), true}, // a burst
		{shot("samsung SM-S918B", "08:12:53.000", false), shot("samsung SM-S918B", "08:12:53.500", true), false},
		{shot("samsung SM-S918B", "08:12:53.000", false), shot("samsung SM-S918B", "08:12:54.000", false), true},
		// Another camera, or none (WhatsApp strips it): the pixels decide.
		{shot("samsung SM-S918B", "08:12:53.000", true), shot("Apple iPhone 15", "08:20:00.000", true), false},
		{shot("samsung SM-S918B", "08:12:53.000", true), shot("", "08:20:00.000", true), false},
	} {
		if got := differentShots(c.a, c.b) != ""; got != c.different {
			t.Errorf("%+v / %+v: different %v, want %v", c.a, c.b, got, c.different)
		}
	}

	// By the time in their names, without camera tags.
	named := func(name string) *picture {
		p := &picture{}
		p.named, p.namedExact = namedTime(name)
		return p
	}
	for _, c := range []struct {
		a, b      string
		different bool
	}{
		{"Screenshot_20251105_161324_Chrome.png", "Screenshot_20251105_161113_Chrome.png", true},
		{"20220909_175106_097.jpg", "20220909_175106_073.jpg", true},
		{"20220604_195852_028_saved.jpg", "20220604_195852_028.jpg", false},
		{"20221110_191432(0).jpg", "20221110_191432(0).dng", false},
		{"DSC_0045_BURST20200311170024427.JPG", "DSC_0046_BURST20200311170024427.JPG", false}, // the burst's start
		{"IMG-20251023-WA0017.jpg", "IMG-20251025-WA0014.jpg", false},                         // when it was sent
		{"20220909_175106.jpg", "IMG-20220909-WA0001.jpg", false},
	} {
		if got := differentShots(named(c.a), named(c.b)) != ""; got != c.different {
			t.Errorf("%s / %s: different %v, want %v", c.a, c.b, got, c.different)
		}
	}

	// By the shot numbers in their names: a copy keeps its name, marks aside.
	for _, c := range []struct {
		a, b      string
		different bool
	}{
		{"DSC_000429.JPG", "DSC_000430.JPG", true},
		{"20220528_084736_05.jpg", "20220528_084736_03.jpg", true},
		{"DSC_000004-נערכה(6).JPG", "DSC_000005-נערכה(6).JPG", true},
		{"DSC_000073-נערכה(1).JPG", "DSC_000073(1).JPG", false},
		{"DSC_0013(2).JPG", "DSC_0013 (2).JPG", false},
		{"20221215_081204.heic", "20221215_081204~20230328-021353.heic", false},
		{"DSC_0441(2).JPG", "dsc_0441.jpg", false},
		{"IMG-20260114-WA0032.jpg", "IMG-20260130-WA0022.jpg", false}, // a photo sent twice
		{"IMG-20221217-WA0020.jpg", "20221217_123948.heic", false},
		{"CAM00428.jpg", "DSC_0428.JPG", false},
	} {
		if got := numberedApart(c.a, c.b); got != c.different {
			t.Errorf("%s / %s: numbered apart %v, want %v", c.a, c.b, got, c.different)
		}
	}
}

func TestComparePictures(t *testing.T) {
	texture := func(seed, dx int) []uint8 {
		grey := make([]uint8, pictureSide*pictureSide)
		for y := range pictureSide {
			for x := range pictureSide {
				v := uint32((x+dx)*7919+y*104729+seed) * 2654435761
				grey[y*pictureSide+x] = uint8(v >> 24)
			}
		}
		return grey
	}
	photo := texture(1, 0)
	copied := append([]uint8(nil), photo...)
	for i := range copied {
		if i%3 == 0 && copied[i] < 255 {
			copied[i]++ // recompressed: a level off here and there
		}
	}
	at := time.Date(2026, 1, 20, 8, 12, 53, 0, time.UTC)
	later := at.Add(time.Second)
	pic := func(grey []uint8, w, h int, camera string, shot *time.Time) *picture {
		return &picture{grey: grey, width: w, height: h, camera: camera, shot: shot}
	}
	for _, c := range []struct {
		name string
		a, b *picture
		want verdict
	}{
		{"resized copy", pic(photo, 4000, 3000, "cam", &at), pic(copied, 1600, 1200, "", nil), verdictSame},
		{"another shot, moved", pic(photo, 4000, 3000, "", nil), pic(texture(1, 3), 4000, 3000, "", nil), verdictDifferent},
		{"a crop", pic(photo, 4000, 3000, "", nil), pic(copied, 3000, 3000, "", nil), verdictDifferent},
		{"same camera, a second later", pic(photo, 4000, 3000, "cam", &at), pic(copied, 4000, 3000, "cam", &later), verdictDifferent},
		{"unreadable", pic(photo, 4000, 3000, "", nil), nil, verdictUnsure},
	} {
		if got, score, why := comparePictures(c.a, c.b); got != c.want {
			t.Errorf("%s: %v (%.1f dB, %s), want %v", c.name, got, score, why, c.want)
		}
	}
	if (&picture{width: 1600, height: 1200}).pixels() >= (&picture{width: 4000, height: 3000}).pixels() {
		t.Error("pixels")
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
