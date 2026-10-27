package media

import (
	"fmt"
	"testing"
)

// SourceOf over real names and paths from the owner's library: imports by where they were found, uploads by the
// phone's folder and what it sent.
func TestSourceOf(t *testing.T) {
	canon := Camera{Make: "Canon", Model: "Canon EOS 80D"}
	samsung := Camera{Make: "samsung", Model: "SM-S931B"}
	marks := NewCameraMarks([]CameraMark{{Make: "canon", Model: "canon eos 80d", Mine: false}, {Make: "samsung", Model: "SM-S931B", Mine: true}})
	imported := func(path string, folders ...string) Facts {
		parts := pathParts(path)
		return Facts{FileName: parts[len(parts)-1], UploadedBy: "import", ImportedFrom: &path, Folders: folders}
	}
	uploaded := func(name, folder, sent string) Facts {
		f := Facts{FileName: name, UploadedBy: "Galaxy S25 (SM-S931B)"}
		if folder != "" {
			f.DeviceFolder = &folder
		}
		if sent != "" {
			f.SentFrom = &sent
		}
		return f
	}
	withCamera := func(f Facts, c Camera) Facts { f.Camera = &c; return f }
	sound := func(f Facts) Facts { f.Kind = KindAudio; return f }
	// The folders an import found that enough files take to be kinds (MinImportedFolderFiles).
	kinds := NewFolderKinds(marks)
	addFolderFiles(kinds, "Petcube", MinImportedFolderFiles)
	addFolderFiles(kinds, "Flowers", MinImportedFolderFiles)
	addFolderFiles(kinds, "SLOW_VIDEO", MinImportedFolderFiles) // a camera's folder all the same
	for _, c := range []struct {
		what   string
		facts  Facts
		source string
		app    string
	}{
		{"a camera's name in a Takeout year folder", imported(`D:\Archive\Takeout\Google Photos\תמונות משנת 2014\DSC_000385.JPG`, "Takeout/Google Photos/תמונות משנת 2014"), SourceCamera, ""},
		{"no clue in a Takeout album", imported(`D:\Archive\Takeout\Google Photos\Trip to Eilat\x.jpg`, "Takeout/Google Photos/Trip to Eilat"), SourceOther, ""},
		{"no clue in a Takeout year folder", imported(`D:\Archive\Takeout\Google Photos\Photos from 2014\x.jpg`, "Takeout/Google Photos/Photos from 2014"), SourceOther, ""},
		{"camera tags in a Takeout year folder", withCamera(imported(`D:\Archive\Takeout\Google Photos\Photos from 2014\x.jpg`, "Takeout/Google Photos/Photos from 2014"), samsung), SourceCamera, ""},
		{"Samsung's burst in archive_new's Camera", imported(`D:\Archive\archive_new\2025-09\Camera\20250928_211937_016.jpg`, "archive_new/2025-09/Camera"), SourceCamera, ""},
		{"no clue in a Camera folder", imported(`D:\Archive\archive_new\2025-09\Camera\x.jpg`, "archive_new/2025-09/Camera"), SourceCamera, ""},
		{"WhatsApp's Private folder", imported(`D:\Archive\phone\WhatsApp\Media\WhatsApp Images\Private\IMG-20260101-WA0002.jpg`, "phone/WhatsApp/Media/WhatsApp Images/Private"), SourceWhatsApp, ""},
		{"WhatsApp's name in Pictures", imported(`D:\Archive\phone\Pictures\IMG-20230115-WA0012.jpg`, "phone/Pictures"), SourceWhatsApp, ""},
		{"WhatsApp's name in DCIM", imported("/b/DCIM/Camera/vid-20230115-wa0003.mp4", "b/DCIM/Camera"), SourceWhatsApp, ""},
		{"a Telegram photo", imported(`D:\Archive\phone\Pictures\photo_2023-01-15_12-34-56.jpg`, "phone/Pictures"), "folder:Telegram", ""},
		{"Telegram's folder", imported(`D:\Archive\phone\Pictures\telegram\x.jpg`, "phone/Pictures/telegram"), "folder:Telegram", ""},
		{"a screenshot in a WhatsApp folder", imported(`D:\Archive\phone\WhatsApp\Media\WhatsApp Images\Screenshot_20240301_030715_Bumble.jpg`, "phone/WhatsApp/Media/WhatsApp Images"), SourceScreenshots, "Bumble"},
		{"a screenshot folder", imported(`D:\Archive\phone\DCIM\Screenshots\x.png`, "phone/DCIM/Screenshots"), SourceScreenshots, ""},
		{"a PNG loose in Pictures", imported(`D:\Archive\phone\Pictures\1740850503547.png`, "phone/Pictures"), SourceOther, ""},
		{"a named folder", imported(`D:\Archive\phone\Pictures\Petcube\x.jpg`, "phone/Pictures/Petcube"), "folder:Petcube", ""},
		{"a dated folder", imported(`D:\Archive\old\2014\x.jpg`, "old/2014"), SourceOther, ""},
		{"right in the searched folder", imported(`D:\Archive\Petcube\x.jpg`, "Petcube"), SourceOther, ""},
		{"a camera marked not mine", withCamera(imported(`D:\Archive\phone\Pictures\x.jpg`, "phone/Pictures"), canon), SourceReceived, ""},
		{"a camera marked not mine, named by a camera", withCamera(imported(`D:\Archive\x\DSC_0001.JPG`, "x"), canon), SourceReceived, ""},
		{"WhatsApp before a camera not mine", withCamera(imported(`D:\Archive\phone\Pictures\IMG-20230115-WA0012.jpg`, "phone/Pictures"), canon), SourceWhatsApp, ""},

		{"Fujifilm's name", imported(`D:\Archive\x\DSCF1234.JPG`, "x"), SourceCamera, ""},
		{"a Camera folder further up", imported(`D:\Archive\phone\DCIM\Camera\Sub\x.jpg`, "phone/DCIM/Camera/Sub"), SourceCamera, ""},
		{"an album in DCIM", imported(`D:\Archive\phone DCIM\Flowers\x.jpg`, "phone DCIM/Flowers"), "folder:Flowers", ""},
		{"right in DCIM", imported(`D:\Archive\phone DCIM\x.jpg`, "phone DCIM"), SourceCamera, ""},
		{"a screen recording", imported(`D:\Archive\phone\Movies\Screen_Recording_20240301-030715_Bumble.mp4`, "phone/Movies"), SourceScreenshots, ""},
		{"upload: the phone's camera", uploaded("20250928_211937.jpg", "DCIM/Camera/", SourceCamera), SourceCamera, ""},
		{"upload: WhatsApp's name in Pictures, sent as other", uploaded("IMG-20260101-WA0002.jpg", "Pictures/", SourceOther), SourceWhatsApp, ""},
		{"upload: a new app's folder:WhatsApp", uploaded("x.jpg", "Pictures/", SourceWhatsApp), SourceWhatsApp, ""},
		{"upload: DCIM/Flowers", uploaded("x.jpg", "DCIM/Flowers/", "folder:Flowers"), "folder:Flowers", ""},
		{"upload: an old app's whatsapp", uploaded("x.jpg", "", OldSourceWhatsApp), SourceWhatsApp, ""},
		{"upload: an app's folder in Pictures survives", uploaded("x.jpg", "Pictures/Petcube/", "folder:Petcube"), "folder:Petcube", ""},
		{"upload: an app's folder in DCIM", uploaded("x.jpg", "DCIM/Petcube/", "folder:Petcube"), "folder:Petcube", ""},
		{"upload: a file loose in Pictures keeps what the phone said", uploaded("1740850503547.png", "Pictures/", "folder:Pinterest"), "folder:Pinterest", ""},
		{"upload: without a folder, what the phone said", uploaded("x.jpg", "", "folder:Petcube"), "folder:Petcube", ""},
		{"upload: nothing said", uploaded("x.jpg", "", ""), SourceOther, ""},
		{"upload: an iPhone's WhatsApp album", uploaded("IMG_0001.JPG", "WhatsApp", SourceWhatsApp), SourceWhatsApp, ""},
		{"upload: a Telegram name in Download", uploaded("photo_2023-01-15_12-34-56.jpg", "Download/", SourceOther), "folder:Telegram", ""},
		{"upload: a screenshot sent as Telegram", uploaded("Screenshot_20240301_030715_Bumble.jpg", "Pictures/Telegram/", "folder:Telegram"), SourceScreenshots, "Bumble"},

		// Sound recordings take no recorder's folder: their sections are calls, recordings and voice messages.
		{"a call in a Call folder", sound(imported(`D:\Archive\phone\not photos or videos\Call\Call@Dana(0501234567)_20200101123456.m4a`, "phone/not photos or videos/Call")), SourceOther, ""},
		{"a call in a Calls folder", sound(imported(`D:\Archive\phone\not photos or videos\Calls\20200101_123456.m4a`, "phone/not photos or videos/Calls")), SourceOther, ""},
		{"a recording in Voice Recorder", sound(imported(`D:\Archive\phone\not photos or videos\Voice Recorder\Voice 001.m4a`, "phone/not photos or videos/Voice Recorder")), SourceOther, ""},
		{"a recording in super_recorder", sound(imported(`D:\Archive\phone\not photos or videos\super_recorder\rec1.mp3`, "phone/not photos or videos/super_recorder")), SourceOther, ""},
		{"a WhatsApp voice message by its name", sound(imported(`D:\Archive\phone\not photos or videos\Call\PTT-20230115-WA0003.opus`, "phone/not photos or videos/Call")), SourceWhatsApp, ""},
		{"a WhatsApp voice message by its folder", sound(imported(`D:\Archive\phone\WhatsApp\Media\WhatsApp Voice Notes\202301\x.opus`, "phone/WhatsApp/Media/WhatsApp Voice Notes/202301")), SourceWhatsApp, ""},
		{"upload: a voice message the phone sent as WhatsApp", sound(uploaded("x.opus", "", SourceWhatsApp)), SourceWhatsApp, ""},
		{"upload: a call recorder's folder", sound(uploaded("x.m4a", "Recordings/Call/", "folder:Call")), SourceOther, ""},

		// Folders a camera makes, at any level, are camera.
		{"100ANDRO", imported(`D:\GoogleDriveTemp\toimport2\DCIM\100ANDRO\x.mp4`, "toimport2/DCIM/100ANDRO"), SourceCamera, ""},
		{"100MEDIA further up", imported(`D:\GoogleDriveTemp\toimport2\100MEDIA\Sub\x.jpg`, "toimport2/100MEDIA/Sub"), SourceCamera, ""},
		{"a DCF folder", imported(`D:\GoogleDriveTemp\toimport2\101MSDCF\x.jpg`, "toimport2/101MSDCF"), SourceCamera, ""},
		{"SLOW_VIDEO, with enough files to be a kind", imported(`D:\GoogleDriveTemp\toimport2\DCIM\SLOW_VIDEO\x.mp4`, "toimport2/DCIM/SLOW_VIDEO"), SourceCamera, ""},
		{"EDITED_VIDEO", imported(`D:\GoogleDriveTemp\toimport2\EDITED_VIDEO\x.mp4`, "toimport2/EDITED_VIDEO"), SourceCamera, ""},
		{"Videocaptures", imported(`D:\GoogleDriveTemp\toimport2\Videocaptures\x.jpg`, "toimport2/Videocaptures"), SourceCamera, ""},
		{"a burst's folder", imported(`D:\GoogleDriveTemp\toimport2\DSC_20190928033805229\x.jpg`, "toimport2/DSC_20190928033805229"), SourceCamera, ""},
		{"a year and a word isn't a DCF folder", imported(`D:\GoogleDriveTemp\toimport2\2019trip\x.jpg`, "toimport2/2019trip"), SourceOther, ""},
		{"upload: DCIM/100ANDRO", uploaded("x.mp4", "DCIM/100ANDRO/", "folder:100ANDRO"), SourceCamera, ""},

		// A folder kind the phone sent is overridden only by a screenshot, WhatsApp or a messaging app.
		{"upload: Petcube's folder with a camera's name and tags", withCamera(uploaded("20250928_211937.jpg", "Pictures/Petcube/", "folder:Petcube"), samsung), "folder:Petcube", ""},
		{"upload: Petcube sent, a camera's name, no folder", uploaded("DSC_0001.JPG", "", "folder:Petcube"), "folder:Petcube", ""},
		{"upload: Petcube sent, a camera not mine", withCamera(uploaded("x.jpg", "", "folder:Petcube"), canon), "folder:Petcube", ""},
		{"upload: Petcube sent, WhatsApp's name", uploaded("IMG-20230115-WA0012.jpg", "Pictures/Petcube/", "folder:Petcube"), SourceWhatsApp, ""},
		{"upload: Petcube's folder, a Telegram name", uploaded("photo_2023-01-15_12-34-56.jpg", "Pictures/Petcube/", "folder:Petcube"), "folder:Telegram", ""},

		// Hebrew screenshot folders.
		{"צילומי מסך", imported(`D:\GoogleDriveTemp\phone\צילומי מסך\x.png`, "phone/צילומי מסך"), SourceScreenshots, ""},
		{"צילומי_מסך", imported(`D:\GoogleDriveTemp\phone\צילומי_מסך\x.png`, "phone/צילומי_מסך"), SourceScreenshots, ""},
		{"הקלטות מסך", imported(`D:\GoogleDriveTemp\phone\הקלטות מסך\x.mp4`, "phone/הקלטות מסך"), SourceScreenshots, ""},
		{"upload: Pictures/צילומי מסך", uploaded("x.png", "Pictures/צילומי מסך/", "folder:צילומי מסך"), SourceScreenshots, ""},

		// A folder only an import found, with fewer files than the minimum.
		{"a one-file folder", imported(`D:\GoogleDriveTemp\טל\x.jpg`, "GoogleDriveTemp/טל"), SourceOther, ""},
		{"New folder", imported(`D:\GoogleDriveTemp\stuff\New folder\x.jpg`, "stuff/New folder"), SourceOther, ""},
	} {
		source, app := SourceOf(c.facts, marks, kinds)
		gotApp := ""
		if app != nil {
			gotApp = *app
		}
		if source != c.source || gotApp != c.app {
			t.Errorf("%s: %s %q, want %s %q", c.what, source, gotApp, c.source, c.app)
		}
	}
}

// addFolderFiles adds n made-up imported files found in a folder named folder to kinds.
func addFolderFiles(kinds *FolderKinds, folder string, n int) {
	for i := range n {
		from := fmt.Sprintf(`D:\GoogleDriveTemp\import\%s\f%d.jpg`, folder, i)
		kinds.Add(fmt.Sprintf("%s-%d", folder, i), Facts{FileName: fmt.Sprintf("f%d.jpg", i), UploadedBy: "import", ImportedFrom: &from, Folders: []string{"import/" + folder}})
	}
}

// A folder only an import found is a kind from MinImportedFolderFiles files, counted ignoring case; a phone's
// folder and the apps the rule knows from one.
func TestSourceOfFolderMinimum(t *testing.T) {
	from := `D:\GoogleDriveTemp\import\נוימן\x.jpg`
	file := Facts{FileName: "x.jpg", UploadedBy: "import", ImportedFrom: &from, Folders: []string{"import/נוימן"}}
	kinds := NewFolderKinds(CameraMarks{})
	addFolderFiles(kinds, "נוימן", MinImportedFolderFiles-1)
	if source, _ := SourceOf(file, CameraMarks{}, kinds); source != SourceOther {
		t.Errorf("%d files: %s, want other", MinImportedFolderFiles-1, source)
	}
	kinds.Add("נוימן-0", file) // the same file again counts once
	if source, _ := SourceOf(file, CameraMarks{}, kinds); source != SourceOther {
		t.Errorf("a file added twice: %s, want other", source)
	}
	kinds.Add("the file itself", file)
	if source, _ := SourceOf(file, CameraMarks{}, kinds); source != "folder:נוימן" {
		t.Errorf("%d files: %s, want folder:נוימן", MinImportedFolderFiles, source)
	}
	if source, _ := SourceOf(file, CameraMarks{}, nil); source != SourceOther {
		t.Errorf("no counts: %s, want other", source)
	}

	// Folder kinds that differ only by case are counted as one.
	calls := NewFolderKinds(CameraMarks{})
	addFolderFiles(calls, "Calls", MinImportedFolderFiles/2)
	addFolderFiles(calls, "calls", MinImportedFolderFiles/2)
	callsFrom := `D:\GoogleDriveTemp\import\calls\x.jpg`
	if source, _ := SourceOf(Facts{FileName: "x.jpg", UploadedBy: "import", ImportedFrom: &callsFrom, Folders: []string{"import/calls"}}, CameraMarks{}, calls); source != "folder:Calls" {
		t.Errorf("Calls and calls: %s, want folder:Calls", source)
	}

	// Never held to it: a phone's folder, and an app the rule knows.
	petcube, sent := "Pictures/Petcube/", "folder:Petcube"
	if source, _ := SourceOf(Facts{FileName: "x.jpg", UploadedBy: "Galaxy S25", DeviceFolder: &petcube, SentFrom: &sent}, CameraMarks{}, nil); source != "folder:Petcube" {
		t.Errorf("a phone's folder: %s, want folder:Petcube", source)
	}
	telegram := `D:\GoogleDriveTemp\import\Telegram\x.jpg`
	if source, _ := SourceOf(Facts{FileName: "x.jpg", UploadedBy: "import", ImportedFrom: &telegram, Folders: []string{"import/Telegram"}}, CameraMarks{}, nil); source != "folder:Telegram" {
		t.Errorf("Telegram: %s, want folder:Telegram", source)
	}
}

// Folder kinds that differ only by case are one kind, spelled as most files spell it (on a tie, the one that sorts
// first), the phone's too.
func TestSourceOfFolderCase(t *testing.T) {
	kinds := NewFolderKinds(CameraMarks{})
	addFolderFiles(kinds, "slicer", MinImportedFolderFiles)
	addFolderFiles(kinds, "Slicer", MinImportedFolderFiles+1)
	for _, folder := range []string{"slicer", "Slicer", "SLICER"} {
		from := `D:\GoogleDriveTemp\import\` + folder + `\x.jpg`
		if source, _ := SourceOf(Facts{FileName: "x.jpg", UploadedBy: "import", ImportedFrom: &from, Folders: []string{"import/" + folder}}, CameraMarks{}, kinds); source != "folder:Slicer" {
			t.Errorf("%s: %s, want folder:Slicer", folder, source)
		}
	}
	device := "Pictures/slicer/"
	if source, _ := SourceOf(Facts{FileName: "x.jpg", UploadedBy: "Galaxy S25", DeviceFolder: &device}, CameraMarks{}, kinds); source != "folder:Slicer" {
		t.Errorf("a phone's slicer: %s, want folder:Slicer", source)
	}
	tie := NewFolderKinds(CameraMarks{})
	addFolderFiles(tie, "calls", MinImportedFolderFiles)
	addFolderFiles(tie, "Calls", MinImportedFolderFiles)
	if got := tie.Spelling("folder:calls"); got != "folder:Calls" {
		t.Errorf("a tie: %s, want folder:Calls", got)
	}
	if got := kinds.Spelling("folder:Unknown"); got != "folder:Unknown" {
		t.Errorf("a kind no file takes: %s", got)
	}
}

func TestNormalizeSource(t *testing.T) {
	for in, want := range map[string]string{
		"whatsapp": SourceWhatsApp, "WhatsApp": SourceWhatsApp, "folder:whatsapp": SourceWhatsApp, "folder:TELEGRAM": "folder:Telegram",
		"Camera": SourceCamera, "folder:Petcube": "folder:Petcube", "received": SourceReceived, "folder:Camera": SourceCamera,
	} {
		if got := NormalizeSource(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

func TestScreenshotApp(t *testing.T) {
	for name, want := range map[string]struct {
		app string
		ok  bool
	}{
		"Screenshot_20240301_030715_Bumble.jpg":                   {"Bumble", true},
		"Screenshot_20221115-220726_Bumble~20221117-191459.jpg":   {"Bumble", true},
		"Screenshot_20220603-144846_Bumble(1).jpg":                {"Bumble", true},
		"Screenshot_20220225-183143_WhatsApp (2).jpg":             {"WhatsApp", true},
		"Screenshot_20231201-155651_Microsoft Remote Desktop.jpg": {"Microsoft Remote Desktop", true},
		"screenshot_20220201-231810_One UI Home(1).PNG":           {"One UI Home", true},
		"Screenshot_20221002-213652_ (1).jpg":                     {"", true},
		"Screenshot_20220917_160207.jpg":                          {"", true},
		"Screenshot_20170608-154416.png":                          {"", true},
		"Screenshot_2015-07-18-03-54-57.png":                      {"", true},
		"IMG-20230115-WA0012.jpg":                                 {"", false},
		"20251101_110819.heic":                                    {"", false},
		"My Screenshot_20240301_030715_Bumble.jpg":                {"", false},
	} {
		if app, ok := ScreenshotApp(name); app != want.app || ok != want.ok {
			t.Errorf("%s: %q %v, want %q %v", name, app, ok, want.app, want.ok)
		}
	}
}

// What a device sent stays in sentFrom when the server's rules change the source, on arrival and when corrected later.
func TestSentFromKeepsWhatTheDeviceSaid(t *testing.T) {
	f := newFixture(t)
	upload := func(data []byte, name string, source *string) Item {
		req := photoRequest(data, name)
		req.Source = source
		f.start(req)
		result, err := f.chunk(data, 0, int64(len(data)))
		if err != nil || result.Item == nil {
			t.Fatal(result, err)
		}
		return *result.Item
	}
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}

	// A screenshot's name wins over what the phone said; what it said is kept.
	shot := upload([]byte("shot"), "Screenshot_20240301_030715_Bumble.jpg", ptr("folder:Telegram")).Sidecar.Facts
	if str(shot.Source) != SourceScreenshots || str(shot.SentFrom) != "folder:Telegram" {
		t.Errorf("screenshot: source %s, sentFrom %s", str(shot.Source), str(shot.SentFrom))
	}
	// Nothing changed: both the same.
	photo := upload([]byte("photo"), "1740850503547.png", ptr(SourceOther))
	if facts := photo.Sidecar.Facts; str(facts.Source) != SourceOther || str(facts.SentFrom) != SourceOther {
		t.Errorf("photo: source %s, sentFrom %s", str(facts.Source), str(facts.SentFrom))
	}
	// Corrected later: sentFrom stays what was sent.
	if _, err := f.store.Relabel(photo.Sha256(), SourceWhatsApp, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Relabel(photo.Sha256(), SourceCamera, nil); err != nil {
		t.Fatal(err)
	}
	if facts := f.store.mustGet(t, photo.Sha256()).Sidecar.Facts; str(facts.Source) != SourceCamera || str(facts.SentFrom) != SourceOther {
		t.Errorf("corrected: source %s, sentFrom %s", str(facts.Source), str(facts.SentFrom))
	}
	// A file placed before sentFrom was written gets its old source there when it is first corrected.
	old := upload([]byte("old"), "1740850503548.png", ptr(SourceOther))
	if err := f.store.Turn(func() error {
		f.store.mu.Lock()
		defer f.store.mu.Unlock()
		item := f.store.items[old.Sha256()]
		sidecar := item.Sidecar
		sidecar.Facts.SentFrom = nil
		return f.store.rewriteSidecar(item, sidecar)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Relabel(old.Sha256(), SourceWhatsApp, nil); err != nil {
		t.Fatal(err)
	}
	if facts := f.store.mustGet(t, old.Sha256()).Sidecar.Facts; str(facts.Source) != SourceWhatsApp || str(facts.SentFrom) != SourceOther {
		t.Errorf("old file: source %s, sentFrom %s", str(facts.Source), str(facts.SentFrom))
	}
}
