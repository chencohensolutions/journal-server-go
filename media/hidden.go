package media

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Hidden is what the user hides on every device (Settings → Private media → Hidden collections in the apps): whole
// sources (any app's folder kind: folder:whatsapp, folder:telegram; compared ignoring case, and an old "whatsapp"
// read as folder:whatsapp; received hides what was sent to the user, "Sent to you"), the screenshots of some apps,
// screenshots whose name says no app, what the nudity scan scored high, and sound recordings. Hidden files stay in the
// data folder as they are; the server leaves them out of what devices see (View). It is kept in settings.json at the
// data folder's root, so it moves with the data.
type Hidden struct {
	Sources               []string `json:"sources,omitempty"`
	ScreenshotApps        []string `json:"screenshotApps,omitempty"`
	ScreenshotsWithoutApp bool     `json:"screenshotsWithoutApp,omitempty"`
	// NudityFrom hides the files the nudity scan scored at this or more, 0 to 1; 0 hides none. Always written, so
	// that off (0) stays off: a settings.json without it, or no settings.json, hides from DefaultNudityFrom.
	NudityFrom float64 `json:"nudityFrom"`
	// Audio is which sound recordings are hidden. Always written, so that shown stays shown: a settings.json without
	// it, or no settings.json, hides all three (DefaultAudio).
	Audio AudioHiding `json:"audio"`
}

// AudioHiding is which of the three kinds of sound recording are hidden (AudioCollectionOf).
type AudioHiding struct {
	Recordings    bool `json:"recordings"`
	Calls         bool `json:"calls"`
	VoiceMessages bool `json:"voiceMessages"`
}

// DefaultAudio hides every sound recording until the user shows them.
var DefaultAudio = AudioHiding{Recordings: true, Calls: true, VoiceMessages: true}

// The audio collections: a phone call, a WhatsApp voice message, and any other recording.
const (
	AudioRecordings    = "recordings"
	AudioCalls         = "calls"
	AudioVoiceMessages = "voiceMessages"
)

// AudioCollectionOf is which audio collection a file is in, or "" for a photo or video: by its kind first (call is
// a call, recording a recording, folder:whatsapp a voice message), then a call by Facts.Call, and any other is a
// recording.
func AudioCollectionOf(facts Facts) string {
	if facts.Kind != KindAudio {
		return ""
	}
	if facts.Source != nil {
		switch NormalizeSource(*facts.Source) {
		case SourceCall:
			return AudioCalls
		case SourceRecording:
			return AudioRecordings
		case SourceWhatsApp:
			return AudioVoiceMessages
		}
	}
	if facts.Call != nil {
		return AudioCalls
	}
	return AudioRecordings
}

// hides reports whether a hides the audio collection named c.
func (a AudioHiding) hides(c string) bool {
	switch c {
	case AudioRecordings:
		return a.Recordings
	case AudioCalls:
		return a.Calls
	case AudioVoiceMessages:
		return a.VoiceMessages
	}
	return false
}

// DefaultNudityFrom is the nudity score from which files are hidden until the user chooses another, or none.
const DefaultNudityFrom = 0.5

// SettingsFormat is the version of settings.json.
const SettingsFormat = 1

type settingsFile struct {
	Format int    `json:"format"`
	Hidden Hidden `json:"hidden"`
}

func (s *Store) settingsPath() string { return filepath.Join(s.root, "settings.json") }

// HidesFile reports whether a file is hidden by its own sidecar: in a hidden collection, or scored at NudityFrom
// or more by the nudity scan (a file not scanned yet isn't).
func (h Hidden) HidesFile(sidecar Sidecar) bool {
	if h.NudityFrom > 0 && sidecar.Detected != nil && sidecar.Detected.Nudity != nil && sidecar.Detected.Nudity.Score >= h.NudityFrom {
		return true
	}
	return h.Hides(sidecar.Facts)
}

// Hides reports whether facts put a file in a hidden collection. Apps are compared ignoring case.
func (h Hidden) Hides(facts Facts) bool {
	if h.Audio.hides(AudioCollectionOf(facts)) {
		return true
	}
	source := SourceOther
	if facts.Source != nil {
		source = *facts.Source
	}
	for _, s := range h.Sources {
		if strings.EqualFold(s, source) {
			return true
		}
	}
	if !strings.EqualFold(source, SourceScreenshots) {
		return false
	}
	if facts.ScreenshotApp == nil {
		return h.ScreenshotsWithoutApp
	}
	for _, app := range h.ScreenshotApps {
		if strings.EqualFold(app, *facts.ScreenshotApp) {
			return true
		}
	}
	return false
}

// normalized is h with apps trimmed, without empty ones or ones that differ only in case (the first spelling is
// kept), sources likewise (an old id read as the one written now, NormalizeSource), both sorted.
func (h Hidden) normalized() Hidden {
	unique := func(values []string) []string {
		seen := map[string]bool{}
		var out []string
		for _, v := range values {
			v = strings.TrimSpace(v)
			if v == "" || seen[strings.ToLower(v)] {
				continue
			}
			seen[strings.ToLower(v)] = true
			out = append(out, v)
		}
		sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
		return out
	}
	sources := make([]string, len(h.Sources))
	for i, s := range h.Sources {
		sources[i] = NormalizeSource(s)
	}
	return Hidden{
		Sources: unique(sources), ScreenshotApps: unique(h.ScreenshotApps), ScreenshotsWithoutApp: h.ScreenshotsWithoutApp,
		NudityFrom: min(max(h.NudityFrom, 0), 1), Audio: h.Audio,
	}
}

// IsEmpty reports whether nothing is hidden.
func (h Hidden) IsEmpty() bool {
	return len(h.Sources) == 0 && len(h.ScreenshotApps) == 0 && !h.ScreenshotsWithoutApp && h.NudityFrom == 0 && h.Audio == AudioHiding{}
}

// Key is a short name for what is hidden ("" when nothing is), by which the change feed tells whether a cursor's
// view is the one in force (View.key).
func (h Hidden) Key() string {
	h = h.normalized()
	if h.IsEmpty() {
		return ""
	}
	for i, app := range h.ScreenshotApps {
		h.ScreenshotApps[i] = strings.ToLower(app)
	}
	for i, s := range h.Sources {
		h.Sources[i] = strings.ToLower(s)
	}
	data, _ := json.Marshal(h)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:6])
}

// readHidden reads settings.json; a data folder without one hides nudity from DefaultNudityFrom and every sound
// recording.
func (s *Store) readHidden() (Hidden, error) {
	data, err := os.ReadFile(s.settingsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return Hidden{NudityFrom: DefaultNudityFrom, Audio: DefaultAudio}, nil
	}
	if err != nil {
		return Hidden{}, err
	}
	var file settingsFile
	if err := json.Unmarshal(data, &file); err != nil {
		return Hidden{}, fmt.Errorf("%s: %w", s.settingsPath(), err)
	}
	if file.Format > SettingsFormat {
		return Hidden{}, fmt.Errorf("%s has format %d, newer than this server's %d", s.settingsPath(), file.Format, SettingsFormat)
	}
	// Written before nudity was hidden here: hidden from the default, as the apps did themselves.
	var written struct {
		Hidden struct {
			NudityFrom *float64     `json:"nudityFrom"`
			Audio      *AudioHiding `json:"audio"`
		} `json:"hidden"`
	}
	if json.Unmarshal(data, &written) == nil {
		if written.Hidden.NudityFrom == nil {
			file.Hidden.NudityFrom = DefaultNudityFrom
		}
		// Written before sound recordings came in: hidden, as they are until the user shows them.
		if written.Hidden.Audio == nil {
			file.Hidden.Audio = DefaultAudio
		}
	}
	return file.Hidden.normalized(), nil
}

// Hidden is what the user hides on every device.
func (s *Store) Hidden() Hidden {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hidden
}

// SetHidden replaces what the user hides, in settings.json (inside a turn, written whole). Devices drop the files
// now hidden and get back the ones shown again at their next sync (see Changes). Nothing in media/ changes.
func (s *Store) SetHidden(h Hidden) error {
	h = h.normalized()
	return s.Turn(func() error {
		data, err := json.MarshalIndent(settingsFile{Format: SettingsFormat, Hidden: h}, "", "  ")
		if err != nil {
			return err
		}
		if err := writeFileAtomic(s.settingsPath(), append(data, '\n')); err != nil {
			return err
		}
		s.mu.Lock()
		s.hidden = h
		s.mu.Unlock()
		return nil
	})
}

// View is what a device may see of its user's files: everything but the hidden collections, and without private
// files when the server keeps them from it (`journal-server private <serial> withhold`).
type View struct {
	WithholdPrivate bool
	Hidden          Hidden
}

// ViewFor is the view of a device that is, or isn't, kept from private files, with what the user hides now.
func (s *Store) ViewFor(withholdPrivate bool) View {
	return View{WithholdPrivate: withholdPrivate, Hidden: s.Hidden()}
}

// key names the view in feed cursors: "w" or "m" (withheld or not), then Hidden.Key.
func (v View) key() string {
	flag := "m"
	if v.WithholdPrivate {
		flag = "w"
	}
	return flag + v.Hidden.Key()
}

// Key names the view, as the feed's cursors do: two views with the same key leave out the same files.
func (v View) Key() string { return v.key() }

// Hides reports whether the view leaves item out. A file is hidden with the file it is an edit of or paired with,
// so the edit of a hidden screenshot is hidden too.
func (s *Store) Hides(v View, item Item) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hides(v, item)
}

// RangeSeen is Range over the files the view doesn't leave out. Callers counting what a device may see use it rather
// than calling Hides from Range's fn: a second read lock taken while a writer waits never comes, and both wait for
// good.
func (s *Store) RangeSeen(v View, fn func(Item)) {
	s.CatchUp()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range s.items {
		if !s.hides(v, *item) {
			fn(*item)
		}
	}
}

// hides is Hides for callers holding s.mu.
func (s *Store) hides(v View, item Item) bool {
	if v.WithholdPrivate && item.Sidecar.IsPrivate() {
		return true
	}
	if v.Hidden.IsEmpty() {
		return false
	}
	if v.Hidden.HidesFile(item.Sidecar) {
		return true
	}
	for _, to := range []*string{item.Sidecar.Facts.VersionOf, item.Sidecar.Facts.PairedWith} {
		if to == nil {
			continue
		}
		if linked, ok := s.items[*to]; ok && v.Hidden.HidesFile(linked.Sidecar) {
			return true
		}
	}
	return false
}

// Collection is one group of files the user can hide: how many files it has, how many of them journal-server import placed,
// and whether it is hidden.
type Collection struct {
	Name     string
	Files    int
	Imported int
	Hidden   bool
}

// count adds a file to the collection.
func (c *Collection) count(facts Facts) {
	c.Files++
	if facts.Imported() {
		c.Imported++
	}
}

// Collections is what the user can hide, with how many files each has (hidden or not): all of WhatsApp,
// screenshots whose name says no app, and the screenshots of each app, most files first. An app is named as most of
// its files spell it; a hidden app with no files left is listed too, so it can be shown again.
func (s *Store) Collections() (whatsapp, withoutApp Collection, apps []Collection) {
	s.CatchUp()
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := s.hidden
	whatsapp = Collection{Name: "WhatsApp", Hidden: h.Hides(Facts{Source: ptr(SourceWhatsApp)})}
	withoutApp = Collection{Name: "", Hidden: h.ScreenshotsWithoutApp}
	counts := map[string]int{}
	imported := map[string]int{}
	spellings := map[string]map[string]int{}
	for _, item := range s.items {
		facts := item.Sidecar.Facts
		switch {
		case facts.Source == nil:
		case strings.EqualFold(*facts.Source, SourceWhatsApp):
			whatsapp.count(facts)
		case *facts.Source == SourceScreenshots && facts.ScreenshotApp == nil:
			withoutApp.count(facts)
		case *facts.Source == SourceScreenshots:
			key := strings.ToLower(*facts.ScreenshotApp)
			counts[key]++
			if facts.Imported() {
				imported[key]++
			}
			if spellings[key] == nil {
				spellings[key] = map[string]int{}
			}
			spellings[key][*facts.ScreenshotApp]++
		}
	}
	for _, app := range h.ScreenshotApps {
		key := strings.ToLower(app)
		if _, ok := counts[key]; !ok {
			counts[key] = 0
			spellings[key] = map[string]int{app: 0}
		}
	}
	for key, n := range counts {
		name := ""
		for spelling, m := range spellings[key] {
			if name == "" || m > spellings[key][name] || m == spellings[key][name] && spelling < name {
				name = spelling
			}
		}
		apps = append(apps, Collection{Name: name, Files: n, Imported: imported[key], Hidden: h.Hides(Facts{Source: ptr(SourceScreenshots), ScreenshotApp: &name})})
	}
	sort.Slice(apps, func(i, j int) bool {
		if apps[i].Files != apps[j].Files {
			return apps[i].Files > apps[j].Files
		}
		return strings.ToLower(apps[i].Name) < strings.ToLower(apps[j].Name)
	})
	return whatsapp, withoutApp, apps
}

// AppCollections is every folder kind in the library (the apps and folders files came from: WhatsApp, Telegram,
// Petcube…), with how many files each has (hidden or not), most files first, named as it is shown (FolderNames:
// "WhatsApp", "Calls"). A hidden one with no files left is listed too, so it can be shown again.
func (s *Store) AppCollections() []Collection {
	s.CatchUp()
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := s.hidden
	counts := map[string]*Collection{}
	add := func(id string, facts *Facts) {
		c := counts[id]
		if c == nil {
			c = &Collection{}
			counts[id] = c
		}
		if facts != nil {
			c.count(*facts)
		}
	}
	for _, item := range s.items {
		if facts := item.Sidecar.Facts; facts.Source != nil && FolderName(*facts.Source) != "" {
			add(NormalizeSource(*facts.Source), &facts)
		}
	}
	for _, source := range h.Sources {
		if FolderName(source) != "" {
			add(NormalizeSource(source), nil)
		}
	}
	names := s.folderNamesLocked()
	apps := make([]Collection, 0, len(counts))
	for id, c := range counts {
		c.Name = names.name(id)
		c.Hidden = h.Hides(Facts{Source: ptr(id)})
		apps = append(apps, *c)
	}
	sort.Slice(apps, func(i, j int) bool {
		if apps[i].Files != apps[j].Files {
			return apps[i].Files > apps[j].Files
		}
		return strings.ToLower(apps[i].Name) < strings.ToLower(apps[j].Name)
	})
	return apps
}

// SentToYouCollection is every file sent to the user (source received: photos from a camera that is not theirs,
// and the like), with how many files it has (hidden or not). Hiding it puts received in Hidden.Sources.
func (s *Store) SentToYouCollection() Collection {
	s.CatchUp()
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := Collection{Name: "Sent to you", Hidden: s.hidden.Hides(Facts{Source: ptr(SourceReceived)})}
	for _, item := range s.items {
		if facts := item.Sidecar.Facts; facts.Source != nil && SameSource(*facts.Source, SourceReceived) {
			c.count(facts)
		}
	}
	return c
}

// AudioCollections is the three audio collections, with how many files each has (hidden or not).
func (s *Store) AudioCollections() (recordings, calls, voiceMessages Collection) {
	s.CatchUp()
	s.mu.RLock()
	defer s.mu.RUnlock()
	a := s.hidden.Audio
	recordings = Collection{Name: AudioRecordings, Hidden: a.Recordings}
	calls = Collection{Name: AudioCalls, Hidden: a.Calls}
	voiceMessages = Collection{Name: AudioVoiceMessages, Hidden: a.VoiceMessages}
	for _, item := range s.items {
		facts := item.Sidecar.Facts
		switch AudioCollectionOf(facts) {
		case AudioRecordings:
			recordings.count(facts)
		case AudioCalls:
			calls.count(facts)
		case AudioVoiceMessages:
			voiceMessages.count(facts)
		}
	}
	return recordings, calls, voiceMessages
}

// NudityCounts is how many files the nudity scan scored at from or more, and how many it hasn't scanned yet (they
// can't be hidden for nudity until it has). Sound recordings are never scanned and not counted.
func (s *Store) NudityCounts(from float64) (files, notScanned int) {
	s.CatchUp()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range s.items {
		if item.Sidecar.Facts.Kind == KindAudio {
			continue
		}
		switch d := item.Sidecar.Detected; {
		case d == nil || d.Nudity == nil:
			notScanned++
		case d.Nudity.Score >= from:
			files++
		}
	}
	return files, notScanned
}

func ptr[T any](v T) *T { return &v }
