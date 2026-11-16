package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/media"
)

// enrolledAppClient is a device of user enrolled by app (EnrollDeviceInput.app), and its serial.
func (e *testEnv) enrolledAppClient(t *testing.T, user, app string) (*http.Client, string) {
	t.Helper()
	code, _ := e.enrollment.Codes.Create(user)
	client, serial, result := e.enrollWithCode(t, code, app)
	if result.Typename != "EnrolledDevice" {
		t.Fatalf("enroll = %+v", result)
	}
	return client, serial
}

// enrollWithCode enrolls app with code, and returns its client and serial when enrolled, and the result.
func (e *testEnv) enrollWithCode(t *testing.T, code, app string) (*http.Client, string, enrollResult) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csr, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "device"}}, key)
	resp := e.graphql(t, e.client(nil), enrollMutation, map[string]any{"input": map[string]any{
		"code": code, "deviceName": "Galaxy S25", "csr": base64.StdEncoding.EncodeToString(csr),
		"keyProtection": "TEE", "app": app,
	}})
	if len(resp.Errors) > 0 {
		t.Fatalf("enrollDevice errors: %+v", resp.Errors)
	}
	var result enrollResult
	json.Unmarshal(resp.Data["enrollDevice"], &result)
	if result.Typename != "EnrolledDevice" {
		return nil, "", result
	}
	certDER, _ := base64.StdEncoding.DecodeString(result.Certificate)
	return e.client(&tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: key}), result.Serial, result
}

// TestOneEnrollmentPerPhone enrolls journal with a code from enroll-code, then Audio Logs with a sibling code journal
// asked for: Audio Logs joins journal's phone (its name and its choices), the code works only for the app it names
// and only once, and revoking either revokes the phone.
func TestOneEnrollmentPerPhone(t *testing.T) {
	env := newTestEnv(t)
	journal, journalSerial := env.enrolledAppClient(t, "hen", "")
	if _, err := env.enrollment.Registry.SetWithholdPrivate(journalSerial, true); err != nil {
		t.Fatal(err)
	}
	if got := string(env.graphql(t, journal, `{ serverInfo { siblingCodes } }`, nil).Data["serverInfo"]); got != `{"siblingCodes":true}` {
		t.Fatalf("serverInfo = %s", got)
	}

	sibling := func(app string) (code, name string) {
		resp := env.graphql(t, journal, `mutation($a: String!) { siblingEnrollmentCode(app: $a) { __typename ... on SiblingCode { code app expiresAt } } }`,
			map[string]any{"a": app})
		if len(resp.Errors) > 0 {
			t.Fatalf("siblingEnrollmentCode: %+v", resp.Errors)
		}
		var result struct {
			Typename string `json:"__typename"`
			Code     string
			App      string
		}
		json.Unmarshal(resp.Data["siblingEnrollmentCode"], &result)
		if result.Typename != "SiblingCode" {
			t.Fatalf("siblingEnrollmentCode = %+v", result)
		}
		return result.Code, result.App
	}

	code, app := sibling("Audio-Logs")
	if app != "audio-logs" {
		t.Errorf("app = %q", app)
	}
	if _, _, result := env.enrollWithCode(t, code, "journal"); result.Reason != "WRONG_APP" {
		t.Fatalf("another app with the code: %+v", result)
	}
	audioLogs, audioSerial, result := env.enrollWithCode(t, code, "audio-logs")
	if audioLogs == nil {
		t.Fatalf("Audio Logs with the sibling code: %+v", result)
	}
	if _, _, again := env.enrollWithCode(t, code, "audio-logs"); again.Reason != "INVALID_CODE" {
		t.Fatalf("the code again: %+v", again)
	}
	phone, _ := env.enrollment.Registry.Find(journalSerial)
	device, _ := env.enrollment.Registry.Find(audioSerial)
	if device.PhoneID() != phone.PhoneID() || device.Name != phone.Name || !device.WithholdPrivate || device.AppName() != "audio-logs" {
		t.Fatalf("Audio Logs' device = %+v, journal's = %+v", device, phone)
	}
	if got := string(env.graphql(t, audioLogs, `{ serverInfo { deviceApps } }`, nil).Data["serverInfo"]); got != `{"deviceApps":true}` {
		t.Fatalf("Audio Logs can't reach the server: %s", got)
	}

	// A third app gets its code from Audio Logs, and joins the same phone.
	resp := env.graphql(t, audioLogs, `mutation { siblingEnrollmentCode(app: "notes") { ... on SiblingCode { code } } }`, nil)
	var third struct{ Code string }
	json.Unmarshal(resp.Data["siblingEnrollmentCode"], &third)
	_, notesSerial, _ := env.enrollWithCode(t, third.Code, "notes")
	if d, _ := env.enrollment.Registry.Find(notesSerial); d.PhoneID() != phone.PhoneID() {
		t.Fatalf("the third app's phone = %q, want %q", d.PhoneID(), phone.PhoneID())
	}

	// Revoking Audio Logs revokes the phone: journal and the third app too.
	if _, err := env.enrollment.Registry.RevokePhone(audioSerial, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, serial := range []string{journalSerial, audioSerial, notesSerial} {
		if !env.enrollment.Registry.IsRevoked(serial) {
			t.Errorf("%s isn't revoked", serial)
		}
	}
}

// TestAudioLogsSeesSoundRecordings enrolls journal and Audio Logs on one phone: the registry tells them apart, and
// only Audio Logs gets the recordings the user hasn't shown, in the feed and from /media; the rest of what the user
// hides stays hidden from it too.
func TestAudioLogsSeesSoundRecordings(t *testing.T) {
	env := newTestEnv(t)
	journal, journalSerial := env.enrolledAppClient(t, "hen", "")
	audioLogs, audioSerial := env.enrolledAppClient(t, "hen", " Audio-Logs ")

	if d, _ := env.enrollment.Registry.Find(journalSerial); d.AppName() != "journal" || d.SeesSoundRecordings() {
		t.Fatalf("journal's device = %+v", d)
	}
	if d, _ := env.enrollment.Registry.Find(audioSerial); d.App != "audio-logs" || !d.SeesSoundRecordings() {
		t.Fatalf("Audio Logs' device = %+v", d)
	}
	if got := string(env.graphql(t, audioLogs, `{ serverInfo { deviceApps } }`, nil).Data["serverInfo"]); got != `{"deviceApps":true}` {
		t.Fatalf("serverInfo = %s", got)
	}

	store := env.library.Store("hen")
	place := func(name, mime string, kind media.Kind, source string) string {
		data := []byte("the bytes of " + name)
		path := filepath.Join(t.TempDir(), name)
		os.WriteFile(path, data, 0o644)
		sum := sha256.Sum256(data)
		sha := hex.EncodeToString(sum[:])
		taken := time.Date(2026, 11, 2, 10, 0, 0, 0, time.FixedZone("", 2*3600))
		req := media.UploadRequest{Sha256: sha, Size: int64(len(data)), FileName: name, MimeType: mime, Kind: kind,
			TakenAt: &taken, Source: &source, UploadedBy: "import"}
		if _, _, err := store.Import(path, req); err != nil {
			t.Fatal(err)
		}
		return sha
	}
	recording := place("audio.aac", "audio/aac", media.KindAudio, media.SourceRecording)
	whatsapp := place("IMG-20261102-WA0001.jpg", "image/jpeg", media.KindPhoto, "folder:whatsapp")
	set := env.graphql(t, journal, `mutation($i: HiddenCollectionsInput!) { setHiddenCollections(input: $i) { recordings { hidden } } }`,
		map[string]any{"i": map[string]any{"whatsapp": true, "screenshotsWithoutApp": false, "screenshotApps": []string{}}})
	if len(set.Errors) > 0 {
		t.Fatalf("setHiddenCollections: %+v", set.Errors)
	}

	feed := `{ changes { changes { path deleted } } }`
	seen := func(client *http.Client) (shown []string) {
		var page struct {
			Changes []struct {
				Path    string
				Deleted bool
			}
		}
		json.Unmarshal(env.graphql(t, client, feed, nil).Data["changes"], &page)
		for _, c := range page.Changes {
			if !c.Deleted {
				shown = append(shown, strings.TrimPrefix(c.Path, "media/"))
			}
		}
		return shown
	}
	if got := seen(journal); len(got) != 0 {
		t.Errorf("journal sees %v", got)
	}
	if got := seen(audioLogs); len(got) != 1 || got[0] != recording {
		t.Errorf("Audio Logs sees %v, want the recording only (not %s)", got, whatsapp)
	}
	status := func(client *http.Client, sha string) int {
		resp, err := client.Get(env.server.URL + "/media/" + sha)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if s := status(journal, recording); s != http.StatusNotFound {
		t.Errorf("journal /media of the recording: %d", s)
	}
	if s := status(audioLogs, recording); s != http.StatusOK {
		t.Errorf("Audio Logs /media of the recording: %d", s)
	}
	if s := status(audioLogs, whatsapp); s != http.StatusNotFound {
		t.Errorf("Audio Logs /media of a hidden WhatsApp photo: %d", s)
	}
}
