package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image/color"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/journal/server-go/media"
)

// TestAJPEGKeptAsJXLOnAPhone keeps a photo the phone uploaded as JPEG as JPEG XL, as scripts/jxl does overnight: the
// phone's feed takes the JPEG back and sends the JPEG XL, which names it, and the JPEG's sha256 still finds it,
// downloads the very same JPEG, and verifies.
func TestAJPEGKeptAsJXLOnAPhone(t *testing.T) {
	vips, _, _ := media.FindVips("")
	x, err := media.LoadJXL(media.JXLLibraries(vips, ""))
	if err != nil {
		t.Skipf("libjxl not installed: %v", err)
	}
	env := newTestEnv(t)
	phone := env.enrolledClient(t)
	store := env.library.Store("hen")
	jpeg := plainJPEG(t, color.RGBA{200, 120, 40, 255})
	jpegSha := env.uploadPhotoWith(t, phone, jpeg, "20251101_110819.jpg", nil)

	type change struct {
		Path    string
		Deleted bool
		Hidden  bool
		Media   *struct {
			Sha256, MimeType, FileName string
			EncodedFrom                *struct{ Sha256, FileName string }
		}
	}
	feed := func(since string) ([]change, string) {
		t.Helper()
		resp := env.graphql(t, phone, `query($s: String) { changes(since: $s) { cursor changes { path deleted hidden
			media { sha256 mimeType fileName encodedFrom { sha256 fileName } } } } }`, map[string]any{"s": since})
		var page struct {
			Cursor  string
			Changes []change
		}
		if err := json.Unmarshal(resp.Data["changes"], &page); err != nil {
			t.Fatalf("changes = %s %+v", resp.Data["changes"], resp.Errors)
		}
		return page.Changes, page.Cursor
	}
	_, cursor := feed("")

	store.JXL = x
	kept, err := store.KeepAsJXL(jpegSha)
	if err != nil {
		t.Fatal(err)
	}
	changes, _ := feed(cursor)
	if len(changes) != 2 || changes[0].Path != "media/"+jpegSha || !changes[0].Deleted ||
		changes[1].Path != "media/"+kept.Sha256() || changes[1].Deleted || changes[1].Media == nil {
		t.Fatalf("feed after keeping it as JPEG XL: %s", mustJSON(changes))
	}
	m := changes[1].Media
	if m.MimeType != "image/jxl" || m.FileName != "20251101_110819.jxl" || m.EncodedFrom == nil ||
		m.EncodedFrom.Sha256 != jpegSha || m.EncodedFrom.FileName != "20251101_110819.jpg" {
		t.Errorf("the JPEG XL: %s", mustJSON(m))
	}

	if got := string(env.graphql(t, phone, `query($h: [Sha256!]!) { missingMedia(hashes: $h) }`,
		map[string]any{"h": []string{jpegSha, kept.Sha256()}}).Data["missingMedia"]); got != `[]` {
		t.Errorf("missingMedia = %s", got)
	}
	get := func(path string) (*http.Response, []byte) {
		t.Helper()
		resp, err := phone.Get(env.server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, body
	}
	if resp, body := get("/media/" + jpegSha); resp.StatusCode != http.StatusOK || !bytes.Equal(body, jpeg) ||
		resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Errorf("GET the JPEG: %d %s, %d bytes, same %v", resp.StatusCode, resp.Header.Get("Content-Type"), len(body), bytes.Equal(body, jpeg))
	}
	if resp, body := get("/media/" + kept.Sha256()); resp.StatusCode != http.StatusOK || !bytes.HasPrefix(body, []byte("\x00\x00\x00\x0cJXL ")) {
		t.Errorf("GET the JPEG XL: %d, %d bytes", resp.StatusCode, len(body))
	} else if sum := sha256.Sum256(body); hex.EncodeToString(sum[:]) != kept.Sha256() {
		t.Error("the JPEG XL's bytes don't have its sha256")
	}
	if resp, _ := get("/media/" + kept.Sha256() + "/preview?w=64&h=48"); resp.StatusCode != http.StatusOK &&
		resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a preview of the JPEG XL: %d", resp.StatusCode)
	} else {
		t.Logf("a preview of the JPEG XL: %d", resp.StatusCode)
	}
	for _, sha := range []string{jpegSha, kept.Sha256()} {
		resp, err := phone.Post(env.server.URL+"/media/"+sha+"/verify", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"result":"ok"`) {
			t.Errorf("verify %s: %d %s", sha[:8], resp.StatusCode, body)
		}
	}

	// The phone names its copy by the JPEG's sha256: marking it private marks the JPEG XL.
	set := env.graphql(t, phone, `mutation($h: [Sha256!]!) { setPrivate(sha256s: $h, private: true) { sha256 private } }`,
		map[string]any{"h": []string{jpegSha}})
	if got := string(set.Data["setPrivate"]); got != `[{"sha256":"`+kept.Sha256()+`","private":true}]` {
		t.Errorf("setPrivate by the JPEG's sha256 = %s %+v", got, set.Errors)
	}

	// A photo the user hides: its JPEG comes as hidden too, so the phone leaves out its own copy of the JPEG.
	store.JXL = nil
	shot := env.uploadPhotoWith(t, phone, plainJPEG(t, color.RGBA{10, 90, 200, 255}), "Screenshot_20240301_030715_Bumble.jpg", nil)
	if set := env.graphql(t, phone, `mutation($i: HiddenCollectionsInput!) { setHiddenCollections(input: $i) { whatsapp { hidden } } }`,
		map[string]any{"i": map[string]any{"whatsapp": false, "screenshotsWithoutApp": false, "screenshotApps": []string{"bumble"}}}); len(set.Errors) > 0 {
		t.Fatalf("setHiddenCollections: %+v", set.Errors)
	}
	_, cursor = feed("")
	store.JXL = x
	keptShot, err := store.KeepAsJXL(shot)
	if err != nil {
		t.Fatal(err)
	}
	changes, _ = feed(cursor)
	if len(changes) != 2 || changes[0].Path != "media/"+shot || !changes[0].Deleted || !changes[0].Hidden ||
		changes[1].Path != "media/"+keptShot.Sha256() || !changes[1].Deleted || !changes[1].Hidden {
		t.Errorf("feed after keeping a hidden photo as JPEG XL: %s", mustJSON(changes))
	}
	if resp, _ := get("/media/" + shot); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET a hidden photo by its JPEG's sha256: %d", resp.StatusCode)
	}
}
