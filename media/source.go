package media

import (
	"path"
	"regexp"
	"strings"
)

// A file's source is its kind: what it is and where it came from, as the apps name it (MediaSource in
// journal-client-kmp). It is one of the fixed ids below, or a folder kind, "folder:" and the folder's name
// ("folder:Telegram", "folder:Petcube"). Ids are compared ignoring case. SourceOf is the only place one is decided.
const (
	SourceCamera      = "camera"
	SourceScreenshots = "screenshots"
	SourceOther       = "other"
	// Photos from a camera the user marked "not mine" (SetCamera): sent to them, shown as "Sent to you".
	SourceReceived = "received"
	// WhatsApp is a folder kind like any other app's since Oct 27, 2026.
	SourceWhatsApp = "folder:WhatsApp"
	// The id WhatsApp had before: read as SourceWhatsApp wherever it is found (sidecars, settings.json, uploads).
	OldSourceWhatsApp = "whatsapp"
)

// folderPrefix starts every folder kind.
const folderPrefix = "folder:"

// knownApps are the apps the rule knows, spelled as their kinds are: a folder kind of one of them is always spelled
// so, whatever case a phone's folder had.
var knownApps = []string{"WhatsApp", "Telegram", "Signal", "Facebook", "Messenger", "Snapchat"}

// FolderSource is the kind of a folder by its name: a name like a fixed id ("Camera", "Screenshots", "Other") is
// that kind, WhatsApp and the other apps the rule knows are spelled as it spells them, and any other name is
// "folder:" and the name. "" for a blank name.
func FolderSource(name string) string {
	name = strings.TrimSpace(name)
	switch strings.ToLower(name) {
	case "":
		return ""
	case SourceCamera, SourceScreenshots, SourceOther:
		return strings.ToLower(name)
	}
	for _, app := range knownApps {
		if strings.EqualFold(app, name) {
			return folderPrefix + app
		}
	}
	return folderPrefix + name
}

// FolderName is the folder's name of a folder kind ("folder:Telegram" → "Telegram"), or "" for any other id.
func FolderName(source string) string {
	if len(source) > len(folderPrefix) && strings.EqualFold(source[:len(folderPrefix)], folderPrefix) {
		return strings.TrimSpace(source[len(folderPrefix):])
	}
	return ""
}

// NormalizeSource is the id as the server writes it: the old "whatsapp" is "folder:WhatsApp", fixed ids are lower
// case, and the apps the rule knows are spelled as it spells them ("folder:telegram" → "folder:Telegram"). Any other
// id is kept as it is.
func NormalizeSource(source string) string {
	trimmed := strings.TrimSpace(source)
	switch lower := strings.ToLower(trimmed); lower {
	case OldSourceWhatsApp:
		return SourceWhatsApp
	case SourceCamera, SourceScreenshots, SourceOther, SourceReceived:
		return lower
	}
	if name := FolderName(trimmed); name != "" {
		if kind := FolderSource(name); kind != "" {
			return kind
		}
	}
	return source
}

// SameSource reports whether two ids name the same kind (ignoring case, and the old "whatsapp").
func SameSource(a, b string) bool { return strings.EqualFold(NormalizeSource(a), NormalizeSource(b)) }

// normalizeFacts maps an old id in facts to the one the server writes now (NormalizeSource), when a sidecar is read,
// so memory holds one id for every file and a later rewrite writes the new one. SentFrom keeps the old id when it had
// none, so nothing a device said is lost.
func normalizeFacts(facts *Facts) {
	if facts.Source == nil {
		return
	}
	normalized := NormalizeSource(*facts.Source)
	if normalized == *facts.Source {
		return
	}
	if facts.SentFrom == nil {
		old := *facts.Source
		facts.SentFrom = &old
	}
	facts.Source = &normalized
}

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

// screenRecordingName is how phones name screen recordings: Samsung's Screen_Recording_20240301-030715_Bumble.mp4,
// and Screen Recording 2024-03-01 at 03.07.15.mov.
var screenRecordingName = regexp.MustCompile(`(?i)^screen[_ ]?recording[_ ]`)

// messagingNames are how messaging apps name the files they save, by the app (its folder kind's name).
var messagingNames = []struct {
	app  string
	name *regexp.Regexp
}{
	// photo_2023-01-15_12-34-56.jpg, video_2023-01-15_12-34-56.mp4
	{"Telegram", regexp.MustCompile(`(?i)^(photo|video)_\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2}`)},
	// signal-2023-01-15-123456.jpg, signal-2023-01-15-12-34-56-789.jpg
	{"Signal", regexp.MustCompile(`(?i)^signal-\d{4}-\d{2}-\d{2}`)},
	// FB_IMG_1673781234567.jpg
	{"Facebook", regexp.MustCompile(`(?i)^fb_img_\d+`)},
	// received_1234567890123456.jpeg
	{"Messenger", regexp.MustCompile(`(?i)^received_\d+`)},
	// Snapchat-1234567890.jpg
	{"Snapchat", regexp.MustCompile(`(?i)^snapchat-\d+`)},
}

// cameraName is how cameras and phones' camera apps name what they take: DSC_000385.JPG, DSCN1234.JPG, DSC01234.JPG,
// Fujifilm's DSCF1234.JPG, Samsung's 20250928_211937.jpg (and 20250928_211937_016.jpg, a burst), IMG_20230115_123456.jpg, PXL_20230115_….jpg,
// VID_20230115_123456.mp4, MOV_0012.mp4, CAM00123.jpg.
var cameraName = regexp.MustCompile(`(?i)^(dsc[_nf]?\d|\d{8}_\d{6}|img_\d{8}|pxl_\d{8}|vid_\d{8}|mov_\d|cam\d)`)

// sharedFolders are Android's shared folders: a file loose in one says nothing of the app that saved it, and the
// folder inside one is the app's (sourceOfFolder in journal-client-kmp).
var sharedFolders = map[string]bool{
	"dcim": true, "pictures": true, "movies": true, "download": true, "documents": true, "music": true, "recordings": true,
	"podcasts": true, "audiobooks": true, "ringtones": true, "alarms": true, "notifications": true,
}

// datedFolder is a folder named for a date or a year ("2025-09", "2014", "Photos from 2014"), which says when, not
// where from.
var datedFolder = regexp.MustCompile(`(?i)^(photos from )?[\d\s._-]+$`)

// SourceOf is the kind of a file: the one rule that decides it, on upload (Store.place), on import and when sources
// are fixed. It reads the file's name, where it was found (the import's importedFrom, or the uploading device's
// deviceFolder), its camera tags and what the device sent (sentFrom, or source on a file placed before sentFrom was
// kept), and the user's cameras. The first that matches:
//
//  1. a screenshot's name: screenshots, with the app on screen when the name has one (app; nil otherwise); a screen
//     recording's name: screenshots;
//  2. a WhatsApp name, or WhatsApp anywhere in the path: folder:WhatsApp;
//  3. a screenshot folder anywhere in the path (Screenshots, Screen recordings): screenshots;
//  4. a messaging app's name or folder (Telegram, Signal, Facebook, Messenger, Snapchat): its folder kind;
//  5. camera tags of a model the user marked "not mine": received;
//  6. a Camera folder anywhere in the path or a DCIM folder right above the file (DCIM/Flowers is Flowers, at 7), a camera's name (DSC_, 20250928_211937, IMG_20230115, PXL_,
//     VID_, MOV_, CAM), or camera tags of any other model: camera;
//  7. a named folder: on upload the device's folder read as the Android app reads it (the folder inside Pictures,
//     Download, DCIM…, else the top one); on import the folder right above the file, unless it is a shared folder,
//     a dated one, the folder the import searched, or anywhere under Google Takeout (its year folders and albums are
//     collections, kept in folders, not where a file came from);
//  8. otherwise: on upload what the device sent (it knows the app that saved a file loose in Pictures/, which the
//     server can't), other when it sent none; on import, other.
//
// On upload the server so overrides the device only on evidence of its own (1–7).
func SourceOf(facts Facts, cameras CameraMarks) (source string, app *string) {
	name := facts.FileName
	if a, ok := ScreenshotApp(name); ok {
		if a != "" {
			return SourceScreenshots, &a
		}
		return SourceScreenshots, nil
	}
	if screenRecordingName.MatchString(name) {
		return SourceScreenshots, nil
	}
	var where string
	switch {
	case facts.Imported() && facts.ImportedFrom != nil:
		where = *facts.ImportedFrom
	case !facts.Imported() && facts.DeviceFolder != nil:
		where = *facts.DeviceFolder
	}
	parts := pathParts(where)
	lowerPath := strings.ToLower(strings.Join(parts, "/"))
	if IsWhatsAppName(name) || strings.Contains(lowerPath, "whatsapp") {
		return SourceWhatsApp, nil
	}
	if strings.Contains(lowerPath, "screenshot") || strings.Contains(lowerPath, "screenrecord") || strings.Contains(lowerPath, "screen record") {
		return SourceScreenshots, nil
	}
	for _, m := range messagingNames {
		if m.name.MatchString(name) || strings.Contains(lowerPath, strings.ToLower(m.app)) {
			return folderPrefix + m.app, nil
		}
	}
	if facts.Camera != nil && cameras.NotMine(*facts.Camera) {
		return SourceReceived, nil
	}
	// On import the path ends with the file's name; a device's folder is the folder itself.
	folders := parts
	if facts.Imported() && len(folders) > 0 {
		folders = folders[:len(folders)-1]
	}
	for _, folder := range folders {
		if strings.EqualFold(folder, "camera") {
			return SourceCamera, nil
		}
	}
	if len(folders) > 0 && strings.Contains(strings.ToLower(folders[len(folders)-1]), "dcim") {
		return SourceCamera, nil
	}
	if cameraName.MatchString(name) || facts.Camera != nil {
		return SourceCamera, nil
	}
	if facts.Imported() {
		if kind := importedFolderSource(folders, lowerPath, facts.Folders); kind != "" {
			return kind, nil
		}
		return SourceOther, nil
	}
	if kind := deviceFolderSource(folders); kind != "" {
		return kind, nil
	}
	sent := facts.SentFrom
	if sent == nil {
		sent = facts.Source
	}
	if sent != nil && strings.TrimSpace(*sent) != "" {
		return NormalizeSource(*sent), nil
	}
	return SourceOther, nil
}

// pathParts is a path's folders (and file name), with either kind of slash, without empty parts or a drive.
func pathParts(p string) []string {
	var parts []string
	for _, part := range strings.Split(strings.ReplaceAll(p, `\`, "/"), "/") {
		if part = strings.TrimSpace(part); part != "" && !strings.HasSuffix(part, ":") {
			parts = append(parts, part)
		}
	}
	return parts
}

// deviceFolderSource is the kind of a device's folder as the Android app tells it (sourceOfFolder in
// journal-client-kmp): the folder inside a shared folder (Pictures/Petcube), none for a file loose in one or under
// Android/, else the top folder (an iPhone's album). "" when the folder names none.
func deviceFolderSource(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	top := strings.ToLower(parts[0])
	switch {
	case sharedFolders[top]:
		if len(parts) > 1 {
			return FolderSource(parts[1])
		}
		return ""
	case top == "android":
		return ""
	}
	return FolderSource(parts[0])
}

// importedFolderSource is the kind of the folder an imported file was found in, right above it, or "" when that
// folder says nothing: a shared folder, a dated one, anything under Google Takeout, or the folder the import searched
// (the file wasn't below it: none of folders, the import's folders from the searched one down, has two parts).
func importedFolderSource(folders []string, lowerPath string, found []string) string {
	if len(folders) == 0 || strings.Contains("/"+lowerPath+"/", "/takeout/") || strings.Contains(lowerPath, "google photos") {
		return ""
	}
	above := folders[len(folders)-1]
	if sharedFolders[strings.ToLower(above)] || datedFolder.MatchString(above) {
		return ""
	}
	below := false
	for _, f := range found {
		if p := pathParts(f); len(p) >= 2 && strings.EqualFold(p[len(p)-1], above) {
			below = true
		}
	}
	if !below {
		return ""
	}
	return FolderSource(above)
}
