package media

import (
	"path"
	"regexp"
	"slices"
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
// sentFrom was kept), the user's cameras, and kinds: the folder kinds the user's other files take (Store.FolderKinds;
// nil is a library without any). The first that matches:
//
//  1. a screenshot's name: screenshots, with the app on screen when the name has one (app; nil otherwise); a screen
//     recording's name: screenshots;
//  2. a WhatsApp name, or WhatsApp anywhere in the path: folder:WhatsApp;
//     a sound recording that isn't WhatsApp's: other (on upload folder:WhatsApp still when the device sent it). A
//     recorder's folder (Call, Voice Recorder) says nothing a sound recording's sections don't (AudioCollectionOf);
//  3. a screenshot folder anywhere in the path (Screenshots, Screen recordings, צילומי מסך, הקלטות מסך): screenshots;
//  4. a messaging app's name or folder (Telegram, Signal, Facebook, Messenger, Snapchat): its folder kind;
//     on upload, a folder kind the device gave: its folder read as the Android app reads it (the folder inside
//     Pictures, Download, DCIM…, else the top one), else what it sent; never a camera's folder (IsCameraFolder). The
//     phone knows the app that saved the file, so its folder kind is overridden by 1–4 only, not by camera tags or a
//     camera's name;
//  5. camera tags of a model the user marked "not mine": received;
//  6. a camera's folder anywhere in the path (Camera, 100ANDRO and other DCF folders, SLOW_VIDEO, EDITED_VIDEO,
//     Videocaptures, a burst's DSC_20190928033805229) or a DCIM folder right above the file (DCIM/Flowers is Flowers,
//     at 7), a camera's name (DSC_, 20250928_211937, IMG_20230115, PXL_, VID_, MOV_, CAM), or camera tags of any
//     other model: camera;
//  7. on import, the folder right above the file, unless it is a shared folder, a dated one, the folder the import
//     searched, or anywhere under Google Takeout (its year folders and albums are collections, kept in folders, not
//     where a file came from); and only when at least MinImportedFolderFiles of the user's files take it (kinds),
//     so a folder of a few files stays other;
//  8. otherwise: on upload what the device sent, other when it sent none; on import, other.
//
// A folder kind is spelled as most of the user's files have it (kinds), so Slicer and slicer are one kind; the apps
// the rule knows are always spelled as it spells them. On upload the server so overrides the device only on evidence
// of its own.
func SourceOf(facts Facts, cameras CameraMarks, kinds *FolderKinds) (source string, app *string) {
	source, app, step := sourceOf(facts, cameras)
	name := FolderName(source)
	if name == "" || knownApp(name) {
		return source, app
	}
	if step == stepImportedFolder && kinds.Found(name) < MinImportedFolderFiles {
		return SourceOther, app
	}
	return kinds.Spelling(source), app
}

// Which step of the rule gave a file its source, where SourceOf treats it on its own.
const (
	stepOther = iota
	stepImportedFolder
)

// sourceOf is the rule without the user's other files: what SourceOf gives before a folder an import found is held
// to the minimum and a folder kind is spelled as the user's files have it, and whether the import's folder gave it.
func sourceOf(facts Facts, cameras CameraMarks) (source string, app *string, step int) {
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
			sent = NormalizeSource(*facts.SentFrom)
		} else if facts.Source != nil {
			sent = NormalizeSource(*facts.Source)
		}
		if n := FolderName(sent); n != "" && IsCameraFolder(n) {
			sent = SourceCamera
		}
		sent = strings.TrimSpace(sent)
	}
	if facts.Kind == KindAudio {
		if SameSource(sent, SourceWhatsApp) {
			return SourceWhatsApp, nil, stepOther
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
			return folderPrefix + m.app, nil, stepOther
		}
	}
	// On import the path ends with the file's name; a device's folder is the folder itself.
	folders := parts
	if !uploaded && len(folders) > 0 {
		folders = folders[:len(folders)-1]
	}
	inCameraFolder := slices.ContainsFunc(folders, IsCameraFolder)
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
			return kind, nil, stepImportedFolder
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

// knownApp reports whether a folder kind's name is one of the apps the rule knows.
func knownApp(name string) bool {
	return slices.ContainsFunc(knownApps, func(app string) bool { return strings.EqualFold(app, name) })
}

// MinImportedFolderFiles is how many of the user's files must take the kind of a folder only an import found before
// it is one: a folder of fewer (someone's name, "New folder", an archive's leftovers) stays other, its name still in
// facts.folders.
const MinImportedFolderFiles = 20

// FolderKinds is what SourceOf needs to know of the user's other files: how many take each folder an import found
// (by its name, ignoring case), and how each folder kind is spelled. Made once for a run (Store.FolderKinds, and Add
// for the files an import is about to place), so every file of the run, a dry run and the real one alike, is decided
// by the same counts. Not safe for Add from several goroutines; reading is.
type FolderKinds struct {
	cameras   CameraMarks
	files     map[string]folderVote     // by sha256, what each file added
	found     map[string]int            // by the folder's name, lower case
	spellings map[string]map[string]int // by the folder's name, lower case: each spelling's files
}

// folderVote is what one file adds to FolderKinds: the folder kind's name it takes, and whether an import's folder
// gave it.
type folderVote struct {
	name  string
	found bool
}

// NewFolderKinds is an empty FolderKinds, deciding files with the user's cameras.
func NewFolderKinds(cameras CameraMarks) *FolderKinds {
	return &FolderKinds{cameras: cameras, files: map[string]folderVote{}, found: map[string]int{}, spellings: map[string]map[string]int{}}
}

// Add counts the folder kind the file with sha256 takes, by the rule before the counts; a file added again counts
// once, by what it has now.
func (k *FolderKinds) Add(sha256 string, facts Facts) {
	if old, ok := k.files[sha256]; ok {
		k.count(old, -1)
		delete(k.files, sha256)
	}
	source, _, step := sourceOf(facts, k.cameras)
	name := FolderName(source)
	if name == "" || knownApp(name) {
		return
	}
	vote := folderVote{name: name, found: step == stepImportedFolder}
	k.files[sha256] = vote
	k.count(vote, 1)
}

func (k *FolderKinds) count(v folderVote, n int) {
	lower := strings.ToLower(v.name)
	if v.found {
		k.found[lower] += n
	}
	if k.spellings[lower] == nil {
		k.spellings[lower] = map[string]int{}
	}
	if k.spellings[lower][v.name] += n; k.spellings[lower][v.name] <= 0 {
		delete(k.spellings[lower], v.name)
	}
}

// Found is how many files take the kind of a folder an import found with this name, ignoring case.
func (k *FolderKinds) Found(name string) int {
	if k == nil {
		return 0
	}
	return k.found[strings.ToLower(strings.TrimSpace(name))]
}

// Spelling is a folder kind as most of the files that take it spell it (on a tie, the one that sorts first, so
// Slicer before slicer); the kind as it is when no file takes it.
func (k *FolderKinds) Spelling(kind string) string {
	name := FolderName(kind)
	if k == nil || name == "" {
		return kind
	}
	best, most := "", 0
	for spelling, n := range k.spellings[strings.ToLower(name)] {
		if n > most || n == most && spelling < best {
			best, most = spelling, n
		}
	}
	if best == "" {
		return kind
	}
	return folderPrefix + best
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

// FolderKinds counts the folder kinds the user's files take, for SourceOf: what a run deciding many files (fixing
// sources, an import) makes once and decides every file by.
func (s *Store) FolderKinds() *FolderKinds {
	s.CatchUp()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.folderKindsLocked()
}

// folderKindsLocked is FolderKinds. Callers hold s.mu.
func (s *Store) folderKindsLocked() *FolderKinds {
	k := NewFolderKinds(s.cameraMarks)
	for sha, item := range s.items {
		k.Add(sha, item.Sidecar.Facts)
	}
	return k
}

// sourceSpellingsLocked is how the user's files spell their folder kinds now, without the rule's counts: all an
// upload needs (a device's folder kind is never held to the minimum), and quick enough to make for each one. Callers
// hold s.mu.
func (s *Store) sourceSpellingsLocked() *FolderKinds {
	k := NewFolderKinds(s.cameraMarks)
	for sha, item := range s.items {
		if source := item.Sidecar.Facts.Source; source != nil {
			if name := FolderName(*source); name != "" && !knownApp(name) {
				vote := folderVote{name: name}
				k.files[sha] = vote
				k.count(vote, 1)
			}
		}
	}
	return k
}
