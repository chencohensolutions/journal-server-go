package media

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// importAudio places a made-up sound recording, as scripts/import does.
func (f *fixture) importAudio(data []byte, name string, call *Call, source string) Item {
	f.t.Helper()
	path := filepath.Join(f.t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		f.t.Fatal(err)
	}
	ms := int64(90_000)
	item, _, err := f.store.Import(path, UploadRequest{
		Sha256: shaOf(data), Size: int64(len(data)), FileName: name, MimeType: "audio/mp4", Kind: KindAudio,
		TakenAt: &takenInTelAviv, DurationMs: &ms, Source: &source, UploadedBy: "import", Call: call,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return item
}

func TestAudioIsHiddenUntilShown(t *testing.T) {
	f := newFixture(t)
	photo := f.upload([]byte("photo"), "20251101_110819.heic")
	contact := "אבא"
	call := f.importAudio([]byte("call"), "Call recording אבא_250206_142813.m4a", &Call{Contact: &contact}, SourceOther)
	recording := f.importAudio([]byte("recording"), "recording-20220925-195923.m4a", nil, SourceOther)
	voice := f.importAudio([]byte("voice"), "PTT-20250208-WA0004.opus", nil, SourceWhatsApp)

	if end := call.Sidecar.Facts.EndedAt(); end == nil || !end.Equal(takenInTelAviv.Add(90*time.Second)) {
		t.Errorf("ended at %v", end)
	}
	if got := AudioCollectionOf(call.Sidecar.Facts) + AudioCollectionOf(recording.Sidecar.Facts) + AudioCollectionOf(voice.Sidecar.Facts) + AudioCollectionOf(photo.Sidecar.Facts); got != "callsrecordingsvoiceMessages" {
		t.Errorf("collections: %s", got)
	}

	// A data folder without settings.json hides all three, and nothing else.
	view := f.store.ViewFor(false)
	items, cursor, _, _ := f.store.Changes("", 10, view)
	if len(items) != 4 || countShown(f.store, view, items) != 1 || f.store.Hides(view, photo) {
		t.Fatalf("by default: %d items, %d shown", len(items), countShown(f.store, view, items))
	}
	if u := f.store.Usage(view); u.Audio != 0 || u.Photos != 1 {
		t.Errorf("usage: %+v", u)
	}
	if u := f.store.Usage(View{}); u.Audio != 3 || u.AudioBytes != int64(len("call")+len("recording")+len("voice")) || u.Photos != 1 {
		t.Errorf("usage of everything: %+v", u)
	}
	recordings, calls, voiceMessages := f.store.AudioCollections()
	// All three came from the import.
	if recordings != (Collection{AudioRecordings, 1, 1, true}) || calls != (Collection{AudioCalls, 1, 1, true}) || voiceMessages != (Collection{AudioVoiceMessages, 1, 1, true}) {
		t.Errorf("collections: %+v %+v %+v", recordings, calls, voiceMessages)
	}
	// Never scanned for nudity, so not waiting for it.
	if _, notScanned := f.store.NudityCounts(0.5); notScanned != 1 {
		t.Errorf("not scanned: %d", notScanned)
	}

	// Calls shown: they come again, and only they.
	f.now = f.now.Add(time.Hour)
	h := f.store.Hidden()
	h.Audio.Calls = false
	if err := f.store.SetHidden(h); err != nil {
		t.Fatal(err)
	}
	view = f.store.ViewFor(false)
	items, _, _, _ = f.store.Changes(cursor, 10, view)
	if len(items) != 1 || items[0].Sha256() != call.Sha256() || f.store.Hides(view, items[0]) {
		t.Fatalf("calls shown: %v", items)
	}

	// Kept as chosen; a settings.json from before sound recordings came in hides them.
	f.reopen()
	if a := f.store.Hidden().Audio; a != (AudioHiding{Recordings: true, Calls: false, VoiceMessages: true}) {
		t.Errorf("reopened: %+v", a)
	}
	old, _ := json.Marshal(map[string]any{"format": 1, "hidden": map[string]any{"nudityFrom": 0.5}})
	if err := os.WriteFile(f.store.settingsPath(), old, 0o644); err != nil {
		t.Fatal(err)
	}
	f.reopen()
	if a := f.store.Hidden().Audio; a != DefaultAudio {
		t.Errorf("an older settings.json: %+v", a)
	}
	if (Hidden{Audio: AudioHiding{Calls: true}}).Key() == (Hidden{}).Key() {
		t.Error("the key doesn't change with the audio switches")
	}
}

func TestSetCall(t *testing.T) {
	f := newFixture(t)
	photo := f.upload([]byte("photo"), "20251101_110819.heic")
	number := "0501234567"
	call := f.importAudio([]byte("call"), "Call recording 0501234567_250726_152307.m4a", &Call{Number: &number}, SourceOther)
	recording := f.importAudio([]byte("recording"), "recording-20220925-195923.m4a", nil, SourceOther)

	f.now = f.now.Add(time.Hour)
	name, spaces := " Dana ", "  "
	item, changed, err := f.store.SetCall(call.Sha256(), Call{Contact: &name, Number: &number})
	if err != nil || !changed || deref(item.Sidecar.Facts.Call.Contact) != "Dana" || deref(item.Sidecar.Facts.Call.Number) != number {
		t.Fatalf("set: %+v %v %v", item.Sidecar.Facts.Call, changed, err)
	}
	if !item.Sidecar.ChangedAt().Equal(f.now.UTC()) {
		t.Errorf("changed at %v, want now", item.Sidecar.ChangedAt())
	}
	if _, changed, _ := f.store.SetCall(call.Sha256(), Call{Contact: &name, Number: &number}); changed {
		t.Error("set again: changed")
	}
	// A recording set so becomes a call; empty values are none.
	item, _, err = f.store.SetCall(recording.Sha256(), Call{Contact: &spaces})
	if err != nil || item.Sidecar.Facts.Call == nil || item.Sidecar.Facts.Call.Contact != nil || AudioCollectionOf(item.Sidecar.Facts) != AudioCalls {
		t.Errorf("recording: %+v %v", item.Sidecar.Facts.Call, err)
	}
	if _, _, err := f.store.SetCall(photo.Sha256(), Call{Contact: &name}); err == nil {
		t.Error("a photo became a call")
	}
	f.reopen()
	if got := f.store.mustGet(t, call.Sha256()).Sidecar.Facts.Call; deref(got.Contact) != "Dana" {
		t.Errorf("reopened: %+v", got)
	}
}

func TestAudioHasNoPicture(t *testing.T) {
	f := newFixture(t)
	item := f.importAudio([]byte("recording"), "recording-20220925-195923.m4a", nil, SourceOther)
	p := &Previews{}
	if _, release, err := p.Get(t.Context(), f.store, item, PreviewSpec{}); !errors.Is(err, ErrNoPicture) {
		t.Errorf("preview: %v", err)
	} else {
		release()
	}
	if _, err := p.Open(t.Context(), f.store, item, 512); !errors.Is(err, ErrNoPicture) {
		t.Errorf("original: %v", err)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
