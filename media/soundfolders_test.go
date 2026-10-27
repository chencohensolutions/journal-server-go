package media

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Merging a sound folder into calls moves its recordings in the background, a correction each and a journal line
// each, counting what is left; taking the mark back brings them back to what they had. Only sidecars change.
func TestMarkingASoundFolderMovesItsFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "media", "2024", "2024-03")
	os.MkdirAll(dir, 0o755)
	write := func(name, facts string) string {
		data := []byte(name)
		sha := shaOf(data)
		os.WriteFile(filepath.Join(dir, name), data, 0o644)
		sidecar := `{"format":1,"sha256":"` + sha + `","facts":{"fileName":"` + name + `","kind":"audio","mimeType":"audio/mp4","size":` +
			`1,"uploadedAt":"2026-10-20T17:50:53Z",` + facts + `}}`
		os.WriteFile(filepath.Join(dir, name+".json"), []byte(sidecar), 0o644)
		return sha
	}
	phone := `"uploadedBy":"Galaxy S25",`
	a := write("Call_1.m4a", phone+`"source":"folder:Call","sentFrom":"folder:Call","deviceFolder":"Recordings/Call/"`)
	b := write("Call_2.m4a", phone+`"source":"folder:call","sentFrom":"folder:call","deviceFolder":"Recordings/call/"`)
	c := write("x.m4a", `"uploadedBy":"import","source":"other","importedFrom":"D:/phone/Call/x.m4a","folders":["phone/Call"]`)
	wa := write("PTT-20240301-WA0001.opus", phone+`"source":"folder:whatsapp","deviceFolder":"Recordings/Call/"`)
	voice := write("v.m4a", phone+`"source":"folder:Voice Recorder","sentFrom":"folder:Voice Recorder","deviceFolder":"Recordings/Voice Recorder/"`)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store, err := OpenStore(root, clock)
	if err != nil {
		t.Fatal(err)
	}
	source := func(sha string) string { return *store.mustGet(t, sha).Sidecar.Facts.Source }

	folders := store.SoundFolders()
	if len(folders) != 2 || folders[0] != (SoundFolder{ID: "folder:call", Name: "Call", Files: 3, Imported: 1}) ||
		folders[1] != (SoundFolder{ID: "folder:voice recorder", Name: "Voice Recorder", Files: 1}) {
		t.Fatalf("sound folders: %+v", folders)
	}

	// Another move runs, so this one waits with its files counted.
	store.soundMoves.run.Lock()
	if err := store.SetSoundFolder("Call", SourceCall); err != nil {
		t.Fatal(err)
	}
	if folders := store.SoundFolders(); folders[0].Pending != 3 || folders[0].As != SourceCall {
		t.Fatalf("pending: %+v", folders)
	}
	store.soundMoves.run.Unlock()
	store.WaitSoundFolderMoves()
	for _, sha := range []string{a, b, c} {
		facts := store.mustGet(t, sha).Sidecar.Facts
		if *facts.Source != SourceCall || facts.CorrectedAt == nil || !facts.CorrectedAt.Equal(now) || AudioCollectionOf(facts) != AudioCalls {
			t.Fatalf("not merged: %+v", facts)
		}
	}
	if sentFrom := store.mustGet(t, c).Sidecar.Facts.SentFrom; sentFrom == nil || *sentFrom != SourceOther {
		t.Fatalf("the import's source before: %v", sentFrom)
	}
	if source(wa) != SourceWhatsApp || source(voice) != "folder:voice recorder" {
		t.Fatalf("others moved: %s %s", source(wa), source(voice))
	}
	if folders := store.SoundFolders(); folders[0].Files != 3 || folders[0].Pending != 0 {
		t.Fatalf("after the move: %+v", folders)
	}
	var file soundFoldersFile
	data, _ := os.ReadFile(filepath.Join(root, "sound-folders.json"))
	if json.Unmarshal(data, &file) != nil || file.Format != 1 || len(file.Folders) != 1 || file.Folders[0] != (SoundFolderMark{"folder:call", "call"}) {
		t.Fatalf("sound-folders.json: %s", data)
	}
	journals, _ := filepath.Glob(filepath.Join(root, "relabels", "*-sound-folders.tsv"))
	if len(journals) != 1 {
		t.Fatalf("journals: %v", journals)
	}
	data, _ = os.ReadFile(journals[0])
	for _, line := range []string{a + "\tsource\tfolder:Call\tcall\n", b + "\tsource\tfolder:call\tcall\n", c + "\tsource\tother\tcall\n", "sha256\tfield\told\tnew\n"} {
		if !strings.Contains(string(data), line) {
			t.Fatalf("the journal lacks %q:\n%s", line, data)
		}
	}
	// The originals are where they were, as they were.
	for _, name := range []string{"Call_1.m4a", "Call_2.m4a", "x.m4a"} {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != name {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}

	// The marks outlive a restart; taking one back brings the files back.
	if store, err = OpenStore(root, clock); err != nil {
		t.Fatal(err)
	}
	if folders := store.SoundFolders(); folders[0].As != SourceCall {
		t.Fatalf("after a restart: %+v", folders)
	}
	now = now.Add(time.Hour)
	if err := store.SetSoundFolder("folder:CALL", ""); err != nil {
		t.Fatal(err)
	}
	store.WaitSoundFolderMoves()
	if source(a) != "folder:call" || source(b) != "folder:call" || source(c) != SourceOther {
		t.Fatalf("not back: %s %s %s", source(a), source(b), source(c))
	}
	if folders := store.SoundFolders(); folders[0].As != "" {
		t.Fatalf("unmarked: %+v", folders)
	}
	if journals, _ := filepath.Glob(filepath.Join(root, "relabels", "*-sound-folders.tsv")); len(journals) != 2 {
		t.Fatalf("journals after unmarking: %v", journals)
	}

	if err := store.SetSoundFolder("camera", SourceCall); err == nil {
		t.Fatal("camera was marked as a sound folder")
	}
	if err := store.SetSoundFolder("Call", SourceOther); err == nil {
		t.Fatal("a folder was merged into other")
	}
}
