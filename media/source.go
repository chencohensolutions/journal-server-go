package media

import (
	"path"
	"regexp"
	"slices"
	"strings"
)

// A file's source is its kind: what it is and where it came from, as the apps name it (MediaSource in
// journal-client-kmp). It is one of the fixed ids below, or a folder kind, "folder:" and the folder's name in lower
// case ("folder:telegram", "folder:petcube", "folder:dine-out"). Ids are written in lower case since Oct 27, 2026 and
// compared ignoring case; a folder kind is shown by its name as most of its files spell it (FolderNames). SourceOf is
// the only place one is decided.
const (
	SourceCamera      = "camera"
	SourceScreenshots = "screenshots"
	SourceOther       = "other"
	// Photos from a camera the user marked "not mine" (SetCamera): sent to them, shown as "Sent to you".
	SourceReceived = "received"
	// Sound recordings: recorded phone calls, and recordings of anything else. A sound file is one of them when the
	// user merged its folder into it (SetSoundFolder), or, for a call, when it is a call (Facts.Call).
	SourceCall      = "call"
	SourceRecording = "recording"
	// WhatsApp is a folder kind like any other app's since Oct 27, 2026.
	SourceWhatsApp = "folder:whatsapp"
	// The id WhatsApp had before: read as SourceWhatsApp wherever it is found (sidecars, settings.json, uploads).
	OldSourceWhatsApp = "whatsapp"
)

// folderPrefix starts every folder kind.
const folderPrefix = "folder:"

// knownApps are the apps the rule knows, spelled as they are shown: a folder kind of one of them is always named so,
// whatever case its files' folders had.
var knownApps = []string{"WhatsApp", "Telegram", "Signal", "Facebook", "Messenger", "Snapchat"}

// FolderSource is the kind of a folder by its name: a name like a fixed id ("Camera", "Screenshots", "Other") is
// that kind, and any other name is "folder:" and the name in lower case ("Dine-Out" is folder:dine-out). "" for a
// blank name.
func FolderSource(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "":
		return ""
	case SourceCamera, SourceScreenshots, SourceOther:
		return name
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

// NormalizeSource is the id as the server writes it, in lower case: the old "whatsapp" and "folder:WhatsApp" are
// "folder:whatsapp", fixed ids are lower case, and so is a folder kind's name ("folder:Calls" → "folder:calls"). Any
// other id is kept as it is.
func NormalizeSource(source string) string {
	trimmed := strings.TrimSpace(source)
	switch lower := strings.ToLower(trimmed); lower {
	case OldSourceWhatsApp:
		return SourceWhatsApp
	case SourceCamera, SourceScreenshots, SourceOther, SourceReceived, SourceCall, SourceRecording:
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

// dcfFolder is the folder a camera makes in DCIM by the DCF standard: a number from 100 to 999 and five capital
// letters, digits or underscores, the first not a digit (100CANON, 101MSDCF, 100_FUJI, and Android's 100ANDRO and
// 100MEDIA). Cameras write it in capitals, so "100years" or "2019trip" isn't one.
var dcfFolder = regexp.MustCompile(`^[1-9]\d{2}[A-Z_][A-Z0-9_]{4}$`)

// cameraFolders are the other folders phones' camera apps make, lower case: slow motion, the gallery's edits of
// videos, and frames captured from a video.
var cameraFolders = map[string]bool{"slow_video": true, "edited_video": true, "videocaptures": true}

// IsCameraFolder reports whether a folder is one a camera or a phone's camera app makes: a Camera folder, a DCF
// folder (100CANON, 100ANDRO, 100MEDIA), SLOW_VIDEO, EDITED_VIDEO, Videocaptures, or a burst's folder named as a
// camera names its files (DSC_20190928033805229, 20250928_211937).
func IsCameraFolder(name string) bool {
	name = strings.TrimSpace(name)
	lower := strings.ToLower(name)
	return lower == SourceCamera || cameraFolders[lower] || dcfFolder.MatchString(name) || cameraName.MatchString(name)
}

// screenshotFolders are what a folder of screenshots or screen recordings has in its name, lower case and without
// spaces, underscores or dashes: Screenshots, Screen recordings, Screen_Recordings, and in Hebrew צילומי מסך
// (screenshots) and הקלטות מסך (screen recordings).
var screenshotFolders = []string{"screenshot", "screenrecord", "צילומימסך", "הקלטותמסך"}

// compactName is a name lower case without spaces, underscores or dashes, as screenshotFolders are written.
var compactName = strings.NewReplacer(" ", "", "_", "", "-", "")

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
// are fixed. It reads the file's name and kind, where it was found (the import's importedFrom, or the uploading
// device's deviceFolder), its camera tags and what the device sent (sentFrom, or source on a file placed before
// sentFrom was kept), the source it has now (for a sound recording), the user's cameras, and kinds: the folder kinds
// the user's other files take and the sound folders the user marked (Store.FolderKinds; nil is a library without
// any). The first that matches:
//
//  1. a screenshot's name: screenshots, with the app on screen when the name has one (app; nil otherwise); a screen
//     recording's name: screenshots;
//  2. a WhatsApp name, or WhatsApp anywhere in the path: folder:whatsapp;
//  3. a sound recording (soundSourceOf): folder:whatsapp when the device sent that; call or recording when the user
//     merged its folder (SoundFolderOf) into calls or recordings (SetSoundFolder); call for a call (Facts.Call);
//     otherwise the source it has (folder:calls, camera, other…), never rewritten; one merged by a mark the user
//     took back goes back to what it had before (sentFrom), else its folder. A new one (no source yet) takes its
//     folder as a photo would at 7 and 8, else other;
//  4. a screenshot folder anywhere in the path (Screenshots, Screen recordings, צילומי מסך, הקלטות מסך): screenshots;
//  5. a messaging app's name or folder (Telegram, Signal, Facebook, Messenger, Snapchat): its folder kind;
//     on upload, a folder kind the device gave: its folder read as the Android app reads it (the folder inside
//     Pictures, Download, DCIM…, else the top one), else what it sent; never a camera's folder (IsCameraFolder). The
//     phone knows the app that saved the file, so its folder kind is overridden by 1–5 only, not by camera tags or a
//     camera's name;
//  6. camera tags of a model the user marked "not mine": received;
//  7. a camera's folder anywhere in the path (Camera, 100ANDRO and other DCF folders, SLOW_VIDEO, EDITED_VIDEO,
//     Videocaptures, a burst's DSC_20190928033805229) or a DCIM folder right above the file (DCIM/Flowers is Flowers,
//     at 8), a camera's name (DSC_, 20250928_211937, IMG_20230115, PXL_, VID_, MOV_, CAM), or camera tags of any
//     other model: camera;
//  8. on import, the folder right above the file, unless it is a shared folder, a dated one, the folder the import
//     searched, or anywhere under Google Takeout (its year folders and albums are collections, kept in folders, not
//     where a file came from); and only when at least MinImportedFolderFiles of the user's files take it (kinds),
//     so a folder of a few files stays other, or is camera when it is anywhere under a DCIM folder (DCIM/Pitch);
//  9. otherwise: on upload what the device sent, other when it sent none; on import, other.
//
// Folder kinds are written in lower case, so Slicer and slicer are one kind. On upload the server so overrides the
// device only on evidence of its own.
func SourceOf(facts Facts, cameras CameraMarks, kinds *FolderKinds) (source string, app *string) {
	source, app, step := sourceOf(facts, cameras, kinds.soundMarks())
	name := FolderName(source)
	if name == "" || knownApp(name) {
		return source, app
	}
	if (step == stepImportedFolder || step == stepImportedFolderInDCIM) && kinds.Found(name) < MinImportedFolderFiles {
		if step == stepImportedFolderInDCIM {
			return SourceCamera, app
		}
		return SourceOther, app
	}
	return source, app
}

// Which step of the rule gave a file its source, where SourceOf treats it on its own.
const (
	stepOther = iota
	stepImportedFolder
	// The import's folder, somewhere under a DCIM folder: camera when it is too small to be a kind.
	stepImportedFolderInDCIM
)

// sourceOf is the rule without the user's other files: what SourceOf gives before a folder an import found is held
// to the minimum, and whether the import's folder gave it.
func sourceOf(facts Facts, cameras CameraMarks, sound SoundFolderMarks) (source string, app *string, step int) {
	name := facts.FileName
	if a, ok := ScreenshotApp(name); ok {
		if a != "" {
			return SourceScreenshots, &a, stepOther
		}
		return SourceScreenshots, nil, stepOther
	}
	if screenRecordingName.MatchString(name) {
		return SourceScreenshots, nil, stepOther
	}
	uploaded := !facts.Imported()
	var where string
	switch {
	case !uploaded && facts.ImportedFrom != nil:
		where = *facts.ImportedFrom
	case uploaded && facts.DeviceFolder != nil:
		where = *facts.DeviceFolder
	}
	parts := pathParts(where)
	lowerPath := strings.ToLower(strings.Join(parts, "/"))
	if IsWhatsAppName(name) || strings.Contains(lowerPath, "whatsapp") {
		return SourceWhatsApp, nil, stepOther
	}
	// What the device sent: sentFrom, or source on a file placed before sentFrom was kept. A camera's folder sent as
	// a folder kind (an older app's folder:100ANDRO) is camera.
	sent := ""
	if uploaded {
		if facts.SentFrom != nil {
			sent = kindOf(*facts.SentFrom)
		} else if facts.Source != nil {
			sent = kindOf(*facts.Source)
		}
	}
	// On import the path ends with the file's name; a device's folder is the folder itself.
	folders := parts
	if !uploaded && len(folders) > 0 {
		folders = folders[:len(folders)-1]
	}
	inCameraFolder := slices.ContainsFunc(folders, IsCameraFolder)
	if facts.Kind == KindAudio {
		if SameSource(sent, SourceWhatsApp) {
			return SourceWhatsApp, nil, stepOther
		}
		if source := soundSourceOf(facts, uploaded, sound); source != "" {
			return source, nil, stepOther
		}
		// A new sound recording: its folder, as a photo's (8 and 9).
		if !uploaded {
			if kind := importedFolderSource(folders, lowerPath, facts.Folders); kind != "" {
				return kind, nil, importedStep(folders)
			}
			return SourceOther, nil, stepOther
		}
		if kind := deviceFolderSource(folders); FolderName(kind) != "" && !inCameraFolder {
			return kind, nil, stepOther
		}
		if sent != "" {
			return sent, nil, stepOther
		}
		return SourceOther, nil, stepOther
	}
	compactPath := compactName.Replace(lowerPath)
	for _, s := range screenshotFolders {
		if strings.Contains(compactPath, s) {
			return SourceScreenshots, nil, stepOther
		}
	}
	for _, m := range messagingNames {
		if m.name.MatchString(name) || strings.Contains(lowerPath, strings.ToLower(m.app)) {
			return FolderSource(m.app), nil, stepOther
		}
	}
	if uploaded {
		if kind := deviceFolderSource(folders); FolderName(kind) != "" && !inCameraFolder {
			return kind, nil, stepOther
		}
		if FolderName(sent) != "" {
			return sent, nil, stepOther
		}
	}
	if facts.Camera != nil && cameras.NotMine(*facts.Camera) {
		return SourceReceived, nil, stepOther
	}
	if inCameraFolder || len(folders) > 0 && strings.Contains(strings.ToLower(folders[len(folders)-1]), "dcim") {
		return SourceCamera, nil, stepOther
	}
	if cameraName.MatchString(name) || facts.Camera != nil {
		return SourceCamera, nil, stepOther
	}
	if !uploaded {
		if kind := importedFolderSource(folders, lowerPath, facts.Folders); kind != "" {
			return kind, nil, importedStep(folders)
		}
		return SourceOther, nil, stepOther
	}
	if kind := deviceFolderSource(folders); kind != "" {
		return kind, nil, stepOther
	}
	if sent != "" {
		return sent, nil, stepOther
	}
	return SourceOther, nil, stepOther
}

// importedStep is the step an import's folder gives a file found in folders: under a DCIM folder or not.
func importedStep(folders []string) int {
	for _, f := range folders {
		if strings.Contains(strings.ToLower(f), "dcim") {
			return stepImportedFolderInDCIM
		}
	}
	return stepImportedFolder
}

// kindOf is an id as the server writes it (NormalizeSource), or camera for a camera's folder given as a folder kind
// (an older app's folder:100ANDRO; told by its own spelling, as a DCF folder is in capitals).
func kindOf(id string) string {
	if n := FolderName(id); n != "" && IsCameraFolder(n) {
		return SourceCamera
	}
	return strings.TrimSpace(NormalizeSource(id))
}

// isSoundKind reports whether an id is one of the sound recordings' fixed kinds, call or recording.
func isSoundKind(id string) bool { return id == SourceCall || id == SourceRecording }

// soundSourceOf is a sound recording's source by the rule's step 3, once WhatsApp is ruled out: the user's mark on
// its folder, a call, or the source it has. "" for a new file, which has none yet.
func soundSourceOf(facts Facts, uploaded bool, sound SoundFolderMarks) string {
	folder := SoundFolderOf(facts)
	if as := sound.As(folder); as != "" {
		return as
	}
	if facts.Call != nil {
		return SourceCall
	}
	stored := ""
	if facts.Source != nil {
		stored = kindOf(*facts.Source)
	}
	if !isSoundKind(stored) {
		return stored
	}
	// Merged into calls or recordings by a mark there is no more: back to what it had before, as the device sent it
	// (a phone's own call or recording stays) or as it was when first relabelled, else its folder.
	if facts.SentFrom != nil {
		if prior := kindOf(*facts.SentFrom); prior != "" && (uploaded || !isSoundKind(prior)) {
			return prior
		}
	}
	if folder != "" {
		return folder
	}
	return SourceOther
}

// SoundFolderOf is the folder kind a sound recording came from, by what its sidecar says of where it was: the folder
// kind the device sent (sentFrom), the device's folder (deviceFolder, read as the Android app reads it), the folder
// the import found it in (importedFrom, right above the file, unless a shared, dated or Takeout folder), the first
// of folders, else the folder kind it has. A file merged into calls or recordings (SetSoundFolder) so still has its
// folder. Never WhatsApp's or a camera's; "" for a file that isn't a sound recording, a WhatsApp voice message (by
// its name or source: no mark moves it), or one without a folder.
func SoundFolderOf(facts Facts) string {
	if facts.Kind != KindAudio || IsWhatsAppName(facts.FileName) || facts.Source != nil && SameSource(*facts.Source, SourceWhatsApp) {
		return ""
	}
	// A folder kind other than WhatsApp's, from a folder that isn't a camera's (told by its spelling as found).
	ok := func(id string, raw ...string) bool {
		return FolderName(id) != "" && !SameSource(id, SourceWhatsApp) && !slices.ContainsFunc(raw, IsCameraFolder)
	}
	if facts.SentFrom != nil {
		if id := kindOf(*facts.SentFrom); ok(id) {
			return id
		}
	}
	if facts.DeviceFolder != nil {
		parts := pathParts(*facts.DeviceFolder)
		if id := deviceFolderSource(parts); ok(id, parts...) {
			return id
		}
	}
	if facts.ImportedFrom != nil {
		parts := pathParts(*facts.ImportedFrom)
		if len(parts) > 0 {
			folders := parts[:len(parts)-1]
			if id := importedFolderSource(folders, strings.ToLower(strings.Join(parts, "/")), facts.Folders); ok(id, folders...) {
				return id
			}
		}
	} else if len(facts.Folders) > 0 {
		if parts := pathParts(facts.Folders[0]); len(parts) > 0 {
			above := parts[len(parts)-1]
			if !sharedFolders[strings.ToLower(above)] && !datedFolder.MatchString(above) {
				if id := FolderSource(above); ok(id, parts...) {
					return id
				}
			}
		}
	}
	if facts.Source != nil {
		if id := kindOf(*facts.Source); ok(id) {
			return id
		}
	}
	return ""
}

// knownApp reports whether a folder kind's name is one of the apps the rule knows.
func knownApp(name string) bool {
	_, ok := knownAppName(name)
	return ok
}

// knownAppName is an app the rule knows spelled as it is shown ("whatsapp" → "WhatsApp").
func knownAppName(name string) (string, bool) {
	for _, app := range knownApps {
		if strings.EqualFold(app, strings.TrimSpace(name)) {
			return app, true
		}
	}
	return "", false
}

// MinImportedFolderFiles is how many of the user's files must take the kind of a folder only an import found before
// it is one: a folder of fewer (someone's name, "New folder", an archive's leftovers) stays other (camera under
// DCIM), its name still in facts.folders.
const MinImportedFolderFiles = 20

// FolderKinds is what SourceOf needs to know of the user's other files and choices: how many files take each folder
// an import found (by its name, ignoring case), and the sound folders the user marked. Made once for a run
// (Store.FolderKinds, and Add for the files an import is about to place), so every file of the run, a dry run and the
// real one alike, is decided by the same counts. Not safe for Add from several goroutines; reading is.
type FolderKinds struct {
	cameras CameraMarks
	sound   SoundFolderMarks
	files   map[string]string // by sha256, the lower-case name of the folder an import found that each file added
	found   map[string]int    // by the folder's name, lower case
}

// NewFolderKinds is an empty FolderKinds, deciding files with the user's cameras and no sound folders marked.
func NewFolderKinds(cameras CameraMarks) *FolderKinds {
	return &FolderKinds{cameras: cameras, files: map[string]string{}, found: map[string]int{}}
}

// soundMarks are the sound folders the user marked; none for nil.
func (k *FolderKinds) soundMarks() SoundFolderMarks {
	if k == nil {
		return SoundFolderMarks{}
	}
	return k.sound
}

// Add counts the folder kind the file with sha256 takes, by the rule before the counts, when an import's folder
// gives it; a file added again counts once, by what it has now.
func (k *FolderKinds) Add(sha256 string, facts Facts) {
	if old, ok := k.files[sha256]; ok {
		k.found[old]--
		delete(k.files, sha256)
	}
	source, _, step := sourceOf(facts, k.cameras, k.sound)
	name := strings.ToLower(FolderName(source))
	if name == "" || knownApp(name) || step != stepImportedFolder && step != stepImportedFolderInDCIM {
		return
	}
	k.files[sha256] = name
	k.found[name]++
}

// Found is how many files take the kind of a folder an import found with this name, ignoring case.
func (k *FolderKinds) Found(name string) int {
	if k == nil {
		return 0
	}
	return k.found[strings.ToLower(strings.TrimSpace(name))]
}

// folderSpelling is how a file's own record spells the folder of the folder kind id: what the device sent
// (sentFrom), its deviceFolder, the import's importedFrom and folders, else its source; "" when none names it.
func folderSpelling(facts Facts, id string) string {
	name := FolderName(id)
	if name == "" {
		return ""
	}
	match := func(candidate string) bool { return strings.ToLower(strings.TrimSpace(candidate)) == name }
	if facts.SentFrom != nil {
		if n := FolderName(*facts.SentFrom); match(n) {
			return n
		}
	}
	var paths []string
	for _, p := range []*string{facts.DeviceFolder, facts.ImportedFrom} {
		if p != nil {
			paths = append(paths, *p)
		}
	}
	paths = append(paths, facts.Folders...)
	for _, p := range paths {
		for _, part := range pathParts(p) {
			if match(part) {
				return part
			}
		}
	}
	if facts.Source != nil {
		if n := FolderName(*facts.Source); match(n) {
			return n
		}
	}
	return ""
}

// folderNames counts how the user's files spell each folder kind, to show it by the spelling most of them have.
type folderNames map[string]map[string]int // by id: each spelling's files

// add counts one file's spelling of the folder kind id ("" adds the id without a spelling).
func (n folderNames) add(id, spelling string) {
	id = NormalizeSource(id)
	if FolderName(id) == "" {
		return
	}
	if n[id] == nil {
		n[id] = map[string]int{}
	}
	if spelling != "" {
		n[id][spelling]++
	}
}

// name is the folder kind's name as it is shown: an app the rule knows as it spells it, else as most files spell it
// (on a tie, the one that sorts first, so Calls before calls), else as the id has it.
func (n folderNames) name(id string) string {
	id = NormalizeSource(id)
	folder := FolderName(id)
	if app, ok := knownAppName(folder); ok {
		return app
	}
	best, most := "", 0
	for spelling, count := range n[id] {
		if count > most || count == most && spelling < best {
			best, most = spelling, count
		}
	}
	if best == "" {
		return folder
	}
	return best
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

// FolderKinds counts the folder kinds the user's files take, with the sound folders the user marked, for SourceOf:
// what a run deciding many files (fixing sources, an import) makes once and decides every file by.
func (s *Store) FolderKinds() *FolderKinds {
	s.CatchUp()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.folderKindsLocked()
}

// folderKindsLocked is FolderKinds. Callers hold s.mu.
func (s *Store) folderKindsLocked() *FolderKinds {
	k := s.marksOnlyLocked()
	for sha, item := range s.items {
		k.Add(sha, item.Sidecar.Facts)
	}
	return k
}

// marksOnlyLocked is a FolderKinds with the user's marks and no counts: all an upload needs (a device's folder kind
// is never held to the minimum), and quick enough to make for each one. Callers hold s.mu.
func (s *Store) marksOnlyLocked() *FolderKinds {
	k := NewFolderKinds(s.cameraMarks)
	k.sound = s.soundMarks
	return k
}

// FolderKindName is a folder kind's id and its name as it is shown.
type FolderKindName struct {
	ID, Name string
}

// FolderNames lists every folder kind the user's files take or came from (a sound recording's folder, SoundFolderOf),
// hidden or marked, by id, each with its name: an app the rule knows as it spells it (WhatsApp), else as most of the
// files spell their folder (Calls for folder:calls).
func (s *Store) FolderNames() []FolderKindName {
	s.CatchUp()
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := s.folderNamesLocked()
	for _, id := range s.hidden.Sources {
		names.add(id, "")
	}
	for id := range s.soundMarks.marks {
		names.add(id, "")
	}
	list := make([]FolderKindName, 0, len(names))
	for id := range names {
		list = append(list, FolderKindName{ID: id, Name: names.name(id)})
	}
	slices.SortFunc(list, func(a, b FolderKindName) int { return strings.Compare(a.ID, b.ID) })
	return list
}

// folderNamesLocked counts how the files spell the folder kind each takes, and a sound recording's folder.
// Callers hold s.mu.
func (s *Store) folderNamesLocked() folderNames {
	names := folderNames{}
	for _, item := range s.items {
		facts := item.Sidecar.Facts
		source := ""
		if facts.Source != nil && FolderName(*facts.Source) != "" {
			source = NormalizeSource(*facts.Source)
			names.add(source, folderSpelling(facts, source))
		}
		if folder := SoundFolderOf(facts); folder != "" && folder != source {
			names.add(folder, folderSpelling(facts, folder))
		}
	}
	return names
}
