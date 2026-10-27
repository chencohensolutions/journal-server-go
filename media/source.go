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

// screenshotName is how Android names screenshots, without the extension: Screenshot_2015-07-18-03-54-57,
// Screenshot_20220917_160207, and since Samsung's One UI with the app on screen after the time:
// Screenshot_20240301_030715_Bumble, Screenshot_20220603-144846_Bumble(1), Screenshot_20221115-220726_Bumble~20221117-191459
// (the ~ and (1) are copies the gallery made).
var screenshotName = regexp.MustCompile(`(?i)^screenshot_(?:\d{8}[-_]\d{6}\d*|\d{4}-\d{2}-\d{2}-\d{2}-\d{2}-\d{2})(?:_(.*?))?(?:~[\d-]+)?\s*(?:\(\d+\))?$`)

// ScreenshotApp reports whether a file name is a screenshot's, and the app on screen when its name says ("Bumble",
// "Microsoft Remote Desktop"; "" when it doesn't).
func ScreenshotApp(fileName string) (app string, ok bool) {
	m := screenshotName.FindStringSubmatch(strings.TrimSuffix(fileName, path.Ext(fileName)))
	if m == nil {
		return "", false
	}
	return strings.TrimSpace(m[1]), true
}

// screenshotFacts sets the source of a file named like a screenshot to screenshots, and its app when the name has
// one, whatever the device or the folder said.
func screenshotFacts(facts *Facts) {
	app, ok := ScreenshotApp(facts.FileName)
	if !ok {
		return
	}
	source := SourceScreenshots
	facts.Source = &source
	if app != "" {
		facts.App = &app
	}
}

// voiceMessageFacts gives a sound recording WhatsApp named (PTT-20250208-WA0004.opus, AUD-…) the source whatsapp
// when none was sent, so it is a voice message.
func voiceMessageFacts(facts *Facts) {
	if facts.Kind == KindAudio && facts.Source == nil && IsWhatsAppName(facts.FileName) {
		source := SourceWhatsApp
		facts.Source = &source
	}
}

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
