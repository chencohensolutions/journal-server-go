package media

import (
	"path"
	"regexp"
	"strings"
)

// The sources a file can come from, as the apps name them (MediaSource in journal-client-kmp).
const (
	SourceCamera      = "camera"
	SourceWhatsApp    = "whatsapp"
	SourceScreenshots = "screenshots"
	SourceOther       = "other"
)

// whatsAppName is how WhatsApp names the files it saves: IMG-20230115-WA0012.jpg, VID-20230115-WA0003.mp4, and
// AUD-, PTT-, STK- and DOC- for voice notes, stickers and documents. Copies keep the name after they leave its
// folders, so the name says where a file came from when its folder doesn't.
var whatsAppName = regexp.MustCompile(`(?i)^(img|vid|aud|ptt|stk|doc)-\d{8}-wa\d+`)

// IsWhatsAppName reports whether a file name is one WhatsApp gives the files it saves.
func IsWhatsAppName(name string) bool { return whatsAppName.MatchString(name) }

// SourceOfPath is where a file came from, by its folder and name, as the Android app names it
// (sourceOfFolder in journal-client-kmp). p may use either kind of slash.
func SourceOfPath(p string) string {
	p = strings.ToLower(strings.ReplaceAll(p, `\`, "/"))
	switch {
	case strings.Contains(p, "screenshot") || strings.Contains(p, "screenrecord") || strings.Contains(p, "screen record"):
		return SourceScreenshots
	case strings.Contains(p, "whatsapp") || IsWhatsAppName(path.Base(p)):
		return SourceWhatsApp
	case strings.Contains(p, "dcim"):
		return SourceCamera
	default:
		return SourceOther
	}
}
