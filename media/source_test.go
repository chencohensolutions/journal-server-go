package media

import "testing"

func TestSourceOfPath(t *testing.T) {
	for path, want := range map[string]string{
		"/b/Phone/DCIM/Camera/x.jpg":                        SourceCamera,
		"/b/Phone/DCIM/Screenshots/x.png":                   SourceScreenshots,
		"/b/WhatsApp/Media/WhatsApp Video/x.mp4":            SourceWhatsApp,
		`D:\b\WhatsApp\Media\WhatsApp Images\Private\x.jpg`: SourceWhatsApp,
		"/b/Pictures/x.jpg":                                 SourceOther,
		"/Hen/phone DCIM/Camera/x.jpg":                      SourceCamera,
		"/b/Pictures/IMG-20230115-WA0012.jpg":               SourceWhatsApp,
		"/b/DCIM/Camera/vid-20230115-wa0003.mp4":            SourceWhatsApp,
		"/b/Pictures/IMG_20230115_123456.jpg":               SourceOther,
		"/b/WA0012/x.jpg":                                   SourceOther,
	} {
		if got := SourceOfPath(path); got != want {
			t.Errorf("%s: %s, want %s", path, got, want)
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
	photo := upload([]byte("photo"), "IMG-20240301-WA0001.jpg", ptr(SourceOther))
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
	old := upload([]byte("old"), "IMG-20240302-WA0002.jpg", ptr(SourceOther))
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
