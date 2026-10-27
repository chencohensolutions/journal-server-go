package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// An app from before apps hides WhatsApp with whatsapp alone, and its old whatsapp source is read as folder:whatsapp;
// a new app hides any app by name, ignoring case.
func TestPrivateSettingsHideAnyApp(t *testing.T) {
	env := newTestEnv(t)
	phone := env.enrolledClient(t)
	wa := env.uploadPhotoWith(t, phone, []byte("from an old app"), "x.jpg", map[string]any{"source": "whatsapp"})
	tg := env.uploadPhotoWith(t, phone, []byte("telegram"), "y.jpg", map[string]any{"source": "folder:Telegram", "folder": "Pictures/Telegram/"})
	pet := env.uploadPhotoWith(t, phone, []byte("petcube"), "z.jpg", map[string]any{"source": "folder:Petcube", "folder": "Pictures/Petcube/"})
	sourceOf := func(sha string) string {
		t.Helper()
		resp := env.graphql(t, phone, `query($s: Sha256!) { media(sha256: $s) { source } }`, map[string]any{"s": sha})
		return string(resp.Data["media"])
	}
	if got := sourceOf(wa) + sourceOf(tg) + sourceOf(pet); got != `{"source":"folder:whatsapp"}{"source":"folder:telegram"}{"source":"folder:petcube"}` {
		t.Fatalf("sources: %s", got)
	}

	const collections = `{ whatsapp { name files hidden } apps { name files imported hidden } }`
	set := func(input map[string]any) string {
		t.Helper()
		input["screenshotsWithoutApp"], input["screenshotApps"] = false, []string{}
		resp := env.graphql(t, phone, `mutation($i: HiddenCollectionsInput!) { setHiddenCollections(input: $i) `+collections+` }`, map[string]any{"i": input})
		if len(resp.Errors) > 0 {
			t.Fatalf("setHiddenCollections: %+v", resp.Errors)
		}
		return string(resp.Data["setHiddenCollections"])
	}
	// The old app: whatsapp, no apps.
	got := set(map[string]any{"whatsapp": true})
	if !strings.Contains(got, `"whatsapp":{"name":"WhatsApp","files":1,"hidden":true}`) || !strings.Contains(got, `{"name":"WhatsApp","files":1,"imported":0,"hidden":true}`) ||
		!strings.Contains(got, `{"name":"Telegram","files":1,"imported":0,"hidden":false}`) {
		t.Fatalf("old app: %s", got)
	}
	if sourceOf(wa) != "null" || sourceOf(tg) == "null" {
		t.Fatal("WhatsApp not hidden")
	}
	// The new app: apps by name, any case; WhatsApp not among them is shown.
	got = set(map[string]any{"apps": []string{"telegram", "folder:PETCUBE"}})
	if !strings.Contains(got, `"whatsapp":{"name":"WhatsApp","files":1,"hidden":false}`) || !strings.Contains(got, `{"name":"Telegram","files":1,"imported":0,"hidden":true}`) ||
		!strings.Contains(got, `{"name":"Petcube","files":1,"imported":0,"hidden":true}`) {
		t.Fatalf("new app: %s", got)
	}
	if sourceOf(wa) == "null" || sourceOf(tg) != "null" || sourceOf(pet) != "null" {
		t.Fatal("apps not hidden as asked")
	}
	// The old app again, leaving the others as they are.
	if got = set(map[string]any{"whatsapp": true}); !strings.Contains(got, `{"name":"Telegram","files":1,"imported":0,"hidden":true}`) ||
		!strings.Contains(got, `"whatsapp":{"name":"WhatsApp","files":1,"hidden":true}`) {
		t.Fatalf("old app again: %s", got)
	}
}

// A screenshot's app is screenshotApp (app, deprecated, says the same), and the phone's folder survives the rule.
func TestUploadsKeepThePhonesFolderAndScreenshotApp(t *testing.T) {
	env := newTestEnv(t)
	phone := env.enrolledClient(t)
	shot := env.uploadPhotoWith(t, phone, []byte("shot"), "Screenshot_20240301_030715_Bumble.jpg", map[string]any{"source": "folder:Telegram", "folder": "Pictures/Telegram/"})
	resp := env.graphql(t, phone, `query($s: Sha256!) { media(sha256: $s) { source screenshotApp app camera { make } } }`, map[string]any{"s": shot})
	if got := string(resp.Data["media"]); got != `{"source":"screenshots","screenshotApp":"Bumble","app":"Bumble","camera":null}` {
		t.Fatalf("screenshot: %s %+v", got, resp.Errors)
	}
}

// cameraDNG is a TIFF naming a camera, as a DNG's header does (media.TiffExif reads it without libvips).
func cameraDNG(maker, model string, salt byte) []byte {
	var b bytes.Buffer
	b.WriteString("MM\x00\x2a")
	binary.Write(&b, binary.BigEndian, uint32(8))
	binary.Write(&b, binary.BigEndian, uint16(2))
	strs := []string{maker + "\x00", model + "\x00"}
	offset := uint32(8 + 2 + 2*12 + 4)
	for i, tag := range []uint16{0x010F, 0x0110} {
		binary.Write(&b, binary.BigEndian, tag)
		binary.Write(&b, binary.BigEndian, uint16(2))
		binary.Write(&b, binary.BigEndian, uint32(len(strs[i])))
		binary.Write(&b, binary.BigEndian, offset)
		offset += uint32(len(strs[i]))
	}
	binary.Write(&b, binary.BigEndian, uint32(0))
	for _, s := range strs {
		b.WriteString(s)
	}
	b.WriteByte(salt)
	return b.Bytes()
}

// Marking a camera not mine answers at once and moves its photos to received in the background, counting what is left.
func TestSetCameraMovesPhotosInTheBackground(t *testing.T) {
	env := newTestEnv(t)
	phone := env.enrolledClient(t)
	var canon []string
	for i := range 3 {
		canon = append(canon, env.uploadPhotoWith(t, phone, cameraDNG("Canon", "Canon EOS 80D", byte(i)), "IMG_0001.dng", map[string]any{"source": "camera"}))
	}
	resp := env.graphql(t, phone, `query($s: Sha256!) { media(sha256: $s) { source camera { make model } } }`, map[string]any{"s": canon[0]})
	if got := string(resp.Data["media"]); got != `{"source":"camera","camera":{"make":"Canon","model":"Canon EOS 80D"}}` {
		t.Fatalf("media: %s %+v", got, resp.Errors)
	}

	const fields = `{ make model files imported years mine marked pending }`
	set := env.graphql(t, phone, `mutation { setCamera(make: "canon", model: "canon eos 80d", mine: false) `+fields+` }`, nil)
	var models []struct {
		Make, Model    string
		Files, Pending int
		Mine, Marked   bool
		Years          []int
	}
	if err := json.Unmarshal(set.Data["setCamera"], &models); err != nil || len(models) != 1 || models[0].Files != 3 || models[0].Mine || !models[0].Marked {
		t.Fatalf("setCamera = %s %+v", set.Data["setCamera"], set.Errors)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		json.Unmarshal(env.graphql(t, phone, `{ cameras `+fields+` }`, nil).Data["cameras"], &models)
		if models[0].Pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still moving: %+v", models)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, sha := range canon {
		if got := string(env.graphql(t, phone, `query($s: Sha256!) { media(sha256: $s) { source } }`, map[string]any{"s": sha}).Data["media"]); got != `{"source":"received"}` {
			t.Fatalf("%s: %s", sha, got)
		}
	}
	info := env.graphql(t, phone, `{ serverInfo { cameras } }`, nil)
	if len(info.Errors) > 0 {
		t.Fatalf("serverInfo: %+v", info.Errors)
	}
}

// Sound folders: every folder sound recordings came from, named as their files spell it; merging one into calls
// answers at once and moves its recordings in the background, counting what is left.
func TestSetSoundFolderMovesRecordingsInTheBackground(t *testing.T) {
	env := newTestEnv(t)
	phone := env.enrolledClient(t)
	audio := func(data, name, source, folder string) string {
		return env.uploadPhotoWith(t, phone, []byte(data), name, map[string]any{"kind": "AUDIO", "mimeType": "audio/mp4",
			"takenAt": "2026-10-20T10:00:00+03:00", "source": source, "folder": folder})
	}
	audio("call one", "a.m4a", "folder:Call", "Recordings/Call/")
	audio("call two", "b.m4a", "folder:call", "Recordings/Call/")
	audio("a recording", "c.m4a", "folder:Voice Recorder", "Recordings/Voice Recorder/")

	const fields = `{ id name files imported as pending }`
	type folder struct {
		ID, Name, As             string
		Files, Imported, Pending int
	}
	var folders []folder
	resp := env.graphql(t, phone, `{ soundFolders `+fields+` }`, nil)
	if err := json.Unmarshal(resp.Data["soundFolders"], &folders); err != nil || len(folders) != 2 ||
		folders[0] != (folder{ID: "folder:call", Name: "Call", As: "NONE", Files: 2}) || folders[1].Name != "Voice Recorder" {
		t.Fatalf("soundFolders = %s %+v", resp.Data["soundFolders"], resp.Errors)
	}
	resp = env.graphql(t, phone, `mutation { setSoundFolder(id: "Call", as: CALL) `+fields+` }`, nil)
	if err := json.Unmarshal(resp.Data["setSoundFolder"], &folders); err != nil || folders[0].As != "CALL" {
		t.Fatalf("setSoundFolder = %s %+v", resp.Data["setSoundFolder"], resp.Errors)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		json.Unmarshal(env.graphql(t, phone, `{ soundFolders `+fields+` }`, nil).Data["soundFolders"], &folders)
		if folders[0].Pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still moving: %+v", folders)
		}
		time.Sleep(20 * time.Millisecond)
	}
	collections := env.graphql(t, phone, `{ hiddenCollections { calls { files } recordings { files } } }`, nil)
	if got := string(collections.Data["hiddenCollections"]); got != `{"calls":{"files":2},"recordings":{"files":1}}` {
		t.Fatalf("hiddenCollections = %s %+v", got, collections.Errors)
	}

	names := env.graphql(t, phone, `{ folderNames { id name } serverInfo { soundFolders } }`, nil)
	if got := string(names.Data["folderNames"]); got != `[{"id":"folder:call","name":"Call"},{"id":"folder:voice recorder","name":"Voice Recorder"}]` {
		t.Fatalf("folderNames = %s %+v", got, names.Errors)
	}
	if got := string(names.Data["serverInfo"]); got != `{"soundFolders":true}` {
		t.Fatalf("serverInfo = %s", got)
	}
	if bad := env.graphql(t, phone, `mutation { setSoundFolder(id: "camera", as: CALL) { id } }`, nil); len(bad.Errors) == 0 {
		t.Fatal("camera was marked as a sound folder")
	}
}
