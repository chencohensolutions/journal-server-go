package media

import "testing"

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
	} {
		source, app := SourceOf(c.facts, marks)
		gotApp := ""
		if app != nil {
			gotApp = *app
		}
		if source != c.source || gotApp != c.app {
			t.Errorf("%s: %s %q, want %s %q", c.what, source, gotApp, c.source, c.app)
		}
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
	if _, err := f.store.SetSource(photo.Sha256(), SourceWhatsApp); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SetSource(photo.Sha256(), SourceCamera); err != nil {
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
	if _, err := f.store.SetSource(old.Sha256(), SourceWhatsApp); err != nil {
		t.Fatal(err)
	}
	if facts := f.store.mustGet(t, old.Sha256()).Sidecar.Facts; str(facts.Source) != SourceWhatsApp || str(facts.SentFrom) != SourceOther {
		t.Errorf("old file: source %s, sentFrom %s", str(facts.Source), str(facts.SentFrom))
	}
}
