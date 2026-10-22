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
