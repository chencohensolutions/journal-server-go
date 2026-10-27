package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/media"
)

// clock gives times a minute apart, so each correction is newer than the one before.
type clock struct{ t time.Time }

func (c *clock) now() time.Time {
	c.t = c.t.Add(time.Minute)
	return c.t
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)} }

// folder is a data folder made up for a test, its sidecars written as older servers and imports left them.
type folder struct {
	t    *testing.T
	root string
	shas map[string]string // by file name
	path map[string]string // the original's path in the data folder, by file name
}

func newFolder(t *testing.T) *folder {
	return &folder{t: t, root: t.TempDir(), shas: map[string]string{}, path: map[string]string{}}
}

// add writes an original and its sidecar with facts (fileName, kind and the rest filled in when missing).
func (f *folder) add(name string, facts map[string]any) string {
	f.t.Helper()
	data := []byte("original " + name)
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	all := map[string]any{"fileName": name, "kind": "photo", "mimeType": "image/jpeg", "size": len(data),
		"takenAt": "2023-01-15T10:00:00Z", "uploadedAt": "2026-10-20T17:50:53Z", "uploadedBy": "import"}
	for k, v := range facts {
		all[k] = v
	}
	rel := "media/2023/2023-01/" + name
	path := filepath.Join(f.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		f.t.Fatal(err)
	}
	sidecar, _ := json.MarshalIndent(map[string]any{"format": 1, "sha256": sha, "facts": all}, "", "  ")
	if err := os.WriteFile(path+".json", sidecar, 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.shas[name], f.path[name] = sha, rel
	return sha
}

// facts is a file's facts as they are on disk.
func (f *folder) facts(name string) map[string]any {
	f.t.Helper()
	var sidecar struct {
		Facts map[string]any `json:"facts"`
	}
	data, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(f.path[name])) + ".json")
	if err != nil {
		f.t.Fatal(err)
	}
	if err := json.Unmarshal(data, &sidecar); err != nil {
		f.t.Fatal(err)
	}
	return sidecar.Facts
}

func (f *folder) sidecarText(name string) string {
	data, _ := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(f.path[name])) + ".json")
	return string(data)
}

// want checks a fact on disk; nil means absent.
func (f *folder) want(name, key string, want any) {
	f.t.Helper()
	got, ok := f.facts(name)[key]
	if want == nil {
		if ok {
			f.t.Errorf("%s: %s is %v, want none", name, key, got)
		}
		return
	}
	if !ok || got != want {
		f.t.Errorf("%s: %s is %v, want %v", name, key, got, want)
	}
}

func (f *folder) settings(sources ...string) {
	f.t.Helper()
	data, _ := json.Marshal(map[string]any{"format": 1, "hidden": map[string]any{"sources": sources, "nudityFrom": 0.5,
		"audio": map[string]bool{"recordings": true, "calls": true, "voiceMessages": true}}})
	if err := os.WriteFile(filepath.Join(f.root, "settings.json"), data, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *folder) hiddenSources() []string {
	f.t.Helper()
	var file struct {
		Hidden struct {
			Sources []string `json:"sources"`
		} `json:"hidden"`
	}
	data, err := os.ReadFile(filepath.Join(f.root, "settings.json"))
	if err != nil {
		f.t.Fatal(err)
	}
	if err := json.Unmarshal(data, &file); err != nil {
		f.t.Fatal(err)
	}
	return file.Hidden.Sources
}

func (f *folder) journals() []string {
	entries, _ := os.ReadDir(filepath.Join(f.root, "relabels"))
	var paths []string
	for _, e := range entries {
		paths = append(paths, filepath.Join(f.root, "relabels", e.Name()))
	}
	return paths
}

const takeout = "D:/Backups/Takeout/Google Photos/Photos from 2023/"

// library is the made-up library the tests relabel: what the import labelled before the rule, and one already right.
func library(t *testing.T) *folder {
	f := newFolder(t)
	f.add("IMG-20230115-WA0012.jpg", map[string]any{"source": "other", "importedFrom": takeout + "IMG-20230115-WA0012.jpg"})
	f.add("IMG-20230116-WA0001.jpg", map[string]any{"source": "whatsapp", "importedFrom": "D:/Backups/WhatsApp/Media/WhatsApp Images/IMG-20230116-WA0001.jpg"})
	f.add("Screenshot_20240301_030715_Bumble.jpg", map[string]any{"source": "other", "importedFrom": takeout + "Screenshot_20240301_030715_Bumble.jpg"})
	f.add("1740850503547.jpg", map[string]any{"source": "other", "importedFrom": takeout + "1740850503547.jpg",
		"camera": map[string]string{"make": "Canon", "model": "Canon EOS 80D"}})
	f.add("photo_2023-01-15_12-34-56.jpg", map[string]any{"source": "other", "importedFrom": takeout + "photo_2023-01-15_12-34-56.jpg"})
	f.add("Screenshot_20220917_160207_Tinder.jpg", map[string]any{"source": "screenshots", "app": "Tinder",
		"importedFrom": "D:/Backups/Screenshots/Screenshot_20220917_160207_Tinder.jpg"})
	f.add("meme.jpg", map[string]any{"source": "other", "importedFrom": takeout + "meme.jpg"})
	f.settings("whatsapp")
	return f
}

func runOrFail(t *testing.T, store *media.Store, o options, now func() time.Time) string {
	t.Helper()
	var out bytes.Buffer
	if err := run(store, o, &out, now); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	return out.String()
}

// snapshot is every file and folder under root, with its size, time and contents.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// os.Stat, not d.Info: on Windows a listing can carry a folder's time from before its last change.
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		entry := info.ModTime().String()
		if !d.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			entry += " " + hex.EncodeToString(sum[:])
		}
		files[path] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestADryRunWritesNothing(t *testing.T) {
	f := library(t)
	// The server has had the folder open (.cache, its index and turns), and placed a file since its index was saved, so
	// a store that may write would save the index again.
	server, err := media.OpenStore(f.root, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Relabel(f.shas["meme.jpg"], media.SourceOther, nil); err != nil {
		t.Fatal(err)
	}
	f.add("IMG-20230117-WA0002.jpg", map[string]any{"source": "camera", "importedFrom": takeout + "IMG-20230117-WA0002.jpg"})
	before := snapshot(t, f.root)

	var out string
	for _, o := range []options{{user: "hen", dryRun: true}, {user: "hen", dryRun: true, list: "other:folder:WhatsApp"},
		{user: "hen", dryRun: true, oldIds: true}} {
		store, err := media.OpenStoreReadOnly(f.root, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		out += runOrFail(t, store, o, time.Now)
	}
	t.Log(out)
	if after := snapshot(t, f.root); !mapsEqual(before, after) {
		t.Fatalf("a dry run wrote in the data folder:\nbefore %v\nafter  %v", before, after)
	}
	for _, want := range []string{"other → folder:WhatsApp: 1", "whatsapp → folder:WhatsApp: 1", "camera → folder:WhatsApp: 1",
		"other → camera: 1", "other → screenshots: 1", "other → folder:Telegram: 1", "stays other: 1",
		"settings.json hidden.sources: whatsapp → folder:WhatsApp", "IMG-20230115-WA0012.jpg", "Bumble"} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run doesn't say %q:\n%s", want, out)
		}
	}
	store, _ := media.OpenStoreReadOnly(f.root, time.Now)
	if _, err := store.Relabel(f.shas["meme.jpg"], media.SourceCamera, nil); err == nil {
		t.Fatal("a store opened to read wrote")
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestARelabelIsUndone(t *testing.T) {
	f := library(t)
	c := newClock()
	store, err := media.OpenStore(f.root, c.now)
	if err != nil {
		t.Fatal(err)
	}
	runOrFail(t, store, options{user: "hen"}, c.now)

	f.want("IMG-20230115-WA0012.jpg", "source", "folder:WhatsApp")
	f.want("IMG-20230115-WA0012.jpg", "sentFrom", "other")
	f.want("IMG-20230116-WA0001.jpg", "source", "folder:WhatsApp")
	f.want("IMG-20230116-WA0001.jpg", "sentFrom", "whatsapp")
	f.want("Screenshot_20240301_030715_Bumble.jpg", "source", "screenshots")
	f.want("Screenshot_20240301_030715_Bumble.jpg", "screenshotApp", "Bumble")
	f.want("1740850503547.jpg", "source", "camera")
	f.want("photo_2023-01-15_12-34-56.jpg", "source", "folder:Telegram")
	f.want("meme.jpg", "source", "other")
	f.want("meme.jpg", "correctedAt", nil)
	f.want("Screenshot_20220917_160207_Tinder.jpg", "app", "Tinder") // already right: not rewritten
	f.want("Screenshot_20220917_160207_Tinder.jpg", "correctedAt", nil)
	if got := f.hiddenSources(); !slices.Equal(got, []string{"folder:WhatsApp"}) {
		t.Fatalf("settings.json after the relabel: %v", got)
	}
	journals := f.journals()
	if len(journals) != 1 {
		t.Fatalf("journals: %v", journals)
	}
	lines, err := readJournal(journals[0])
	if err != nil || len(lines) != 8 { // settings, 5 sources, Bumble's app and its key
		t.Fatalf("journal: %v %v", lines, err)
	}
	relabelled := f.facts("IMG-20230115-WA0012.jpg")["correctedAt"].(string)
	// A second run finds nothing to do and writes no journal.
	if out := runOrFail(t, store, options{user: "hen"}, c.now); !strings.Contains(out, "0 would change") && !strings.Contains(out, "0 change") {
		t.Fatalf("second run: %s", out)
	}
	if len(f.journals()) != 1 {
		t.Fatal("a run that changed nothing wrote a journal")
	}

	// Meanwhile: a note on one file, Telegram hidden, and a later correction of another.
	wa := f.path["IMG-20230115-WA0012.jpg"]
	if err := store.Turn(func() error {
		path := filepath.Join(f.root, filepath.FromSlash(wa)) + ".json"
		data, _ := os.ReadFile(path)
		var sidecar map[string]any
		json.Unmarshal(data, &sidecar)
		sidecar["note"] = map[string]any{"caption": "beach"}
		data, _ = json.MarshalIndent(sidecar, "", "  ")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return err
		}
		return store.Reload(wa)
	}); err != nil {
		t.Fatal(err)
	}
	hidden := store.Hidden()
	hidden.Sources = append(hidden.Sources, "folder:Telegram")
	if err := store.SetHidden(hidden); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Relabel(f.shas["photo_2023-01-15_12-34-56.jpg"], media.SourceCamera, nil); err != nil {
		t.Fatal(err)
	}

	runOrFail(t, store, options{user: "hen", undo: journals[0]}, c.now)
	f.want("IMG-20230115-WA0012.jpg", "source", "other")
	f.want("IMG-20230116-WA0001.jpg", "source", "whatsapp")
	f.want("Screenshot_20240301_030715_Bumble.jpg", "source", "other")
	f.want("Screenshot_20240301_030715_Bumble.jpg", "screenshotApp", nil)
	f.want("1740850503547.jpg", "source", "other")
	f.want("photo_2023-01-15_12-34-56.jpg", "source", "camera") // corrected since: kept
	f.want("meme.jpg", "correctedAt", nil)
	if !strings.Contains(f.sidecarText("IMG-20230115-WA0012.jpg"), `"caption": "beach"`) {
		t.Fatalf("the note is lost: %s", f.sidecarText("IMG-20230115-WA0012.jpg"))
	}
	if undone := f.facts("IMG-20230115-WA0012.jpg")["correctedAt"].(string); undone <= relabelled {
		t.Fatalf("correctedAt %s after the undo, %s after the relabel", undone, relabelled)
	}
	if got := f.hiddenSources(); !slices.Equal(got, []string{"folder:Telegram", "whatsapp"}) {
		t.Fatalf("settings.json after the undo: %v", got)
	}
	again, err := media.OpenStore(f.root, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	telegram := "folder:Telegram"
	if !again.Hidden().Hides(media.Facts{Source: &telegram}) {
		t.Fatal("Telegram, hidden after the relabel, is shown after the undo")
	}
	if len(f.journals()) != 2 {
		t.Fatalf("the undo wrote no journal of its own: %v", f.journals())
	}
}

func TestToOldIdsAndBack(t *testing.T) {
	f := library(t)
	c := newClock()
	store, err := media.OpenStore(f.root, c.now)
	if err != nil {
		t.Fatal(err)
	}
	runOrFail(t, store, options{user: "hen"}, c.now)
	// After the relabel: a file a phone uploads as folder:WhatsApp, and a camera marked not mine.
	sha := f.add("IMG-20260101-WA0002.jpg", map[string]any{"source": "folder:WhatsApp", "sentFrom": "folder:WhatsApp", "uploadedBy": "pixel-8"})
	if err := store.Turn(func() error { return store.Reload(f.path["IMG-20260101-WA0002.jpg"]) }); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get(sha); !ok {
		t.Fatal("the upload isn't indexed")
	}
	if _, err := store.Relabel(f.shas["1740850503547.jpg"], media.SourceReceived, nil); err != nil {
		t.Fatal(err)
	}

	runOrFail(t, store, options{user: "hen", oldIds: true}, c.now)
	for _, name := range []string{"IMG-20230115-WA0012.jpg", "IMG-20230116-WA0001.jpg", "IMG-20260101-WA0002.jpg"} {
		f.want(name, "source", "whatsapp")
	}
	f.want("1740850503547.jpg", "source", "camera")
	f.want("Screenshot_20240301_030715_Bumble.jpg", "app", "Bumble")
	f.want("Screenshot_20240301_030715_Bumble.jpg", "screenshotApp", nil)
	f.want("Screenshot_20220917_160207_Tinder.jpg", "app", "Tinder")
	f.want("photo_2023-01-15_12-34-56.jpg", "source", "folder:Telegram")
	if got := f.hiddenSources(); !slices.Equal(got, []string{"whatsapp"}) {
		t.Fatalf("settings.json: %v", got)
	}
	// The new server still reads them all as it did.
	reopened, err := media.OpenStore(f.root, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if item, _ := reopened.Get(f.shas["Screenshot_20240301_030715_Bumble.jpg"]); item.Sidecar.Facts.ScreenshotApp == nil || *item.Sidecar.Facts.ScreenshotApp != "Bumble" {
		t.Fatalf("Bumble's app as the server reads it: %+v", item.Sidecar.Facts)
	}

	journals := f.journals()
	if len(journals) != 2 {
		t.Fatalf("journals: %v", journals)
	}
	runOrFail(t, store, options{user: "hen", undo: journals[1]}, c.now)
	for _, name := range []string{"IMG-20230115-WA0012.jpg", "IMG-20230116-WA0001.jpg", "IMG-20260101-WA0002.jpg"} {
		f.want(name, "source", "folder:WhatsApp")
	}
	f.want("1740850503547.jpg", "source", "received")
	f.want("Screenshot_20240301_030715_Bumble.jpg", "screenshotApp", "Bumble")
	f.want("Screenshot_20240301_030715_Bumble.jpg", "app", nil)
	f.want("Screenshot_20220917_160207_Tinder.jpg", "app", "Tinder") // it had app before --to-old-ids
	if got := f.hiddenSources(); !slices.Equal(got, []string{"folder:WhatsApp"}) {
		t.Fatalf("settings.json after undoing: %v", got)
	}
}

func TestAStoppedRunCanBeUndone(t *testing.T) {
	f := library(t)
	c := newClock()
	store, err := media.OpenStore(f.root, c.now)
	if err != nil {
		t.Fatal(err)
	}
	p, err := newPlan(store, modeRelabel, nil)
	if err != nil || len(p.changes) != 5 {
		t.Fatalf("plan: %v %v", p, err)
	}
	// The third file's sidecar goes away before its turn: the run stops there.
	third := p.changes[2].item
	if err := os.Remove(filepath.Join(f.root, filepath.FromSlash(third.Path)) + ".json"); err != nil {
		t.Fatal(err)
	}
	j := newJournal(store, "hen", modeRelabel, c.now)
	files, err := p.apply(store, j)
	j.close()
	if err == nil || files != 2 {
		t.Fatalf("stopped run: %d files, %v", files, err)
	}
	lines, err := readJournal(j.path)
	if err != nil {
		t.Fatal(err)
	}
	changed := map[string]bool{}
	for _, l := range lines {
		changed[l.sha] = true
	}
	if len(changed) != 3 || !changed[settingsFile] { // settings.json and two files
		t.Fatalf("journal of the stopped run: %v", lines)
	}
	for _, c := range p.changes[:2] {
		if !changed[c.item.Sha256()] {
			t.Fatalf("%s changed but isn't in the journal", c.item.Path)
		}
	}

	runOrFail(t, store, options{user: "hen", undo: j.path}, c.now)
	for _, c := range p.changes[:2] {
		name := c.item.Sidecar.Facts.FileName
		f.want(name, "source", c.before.Source)
	}
	if got := f.hiddenSources(); !slices.Equal(got, []string{"whatsapp"}) {
		t.Fatalf("settings.json after undoing: %v", got)
	}
}

func TestJournalValuesRoundTrip(t *testing.T) {
	for _, s := range []string{"", "plain", "tab\there", `back\slash`, "new\nline", `\t literal`} {
		if got := unescape(escape(s)); got != s {
			t.Errorf("%q came back as %q", s, got)
		}
	}
	if count(28900) != "28,900" || count(7) != "7" || count(1234567) != "1,234,567" {
		t.Fatal(count(28900), count(1234567))
	}
}
