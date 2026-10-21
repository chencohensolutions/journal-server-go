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
