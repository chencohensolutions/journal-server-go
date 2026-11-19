package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/peers"
)

// A swap's name becomes a folder that is deleted whole, so it is never taken as a path: revokeSwap and removeSwap
// with "..", a separator or a drive in the name delete nothing.
func TestSwapNamesAreNeverPaths(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "archive", "peers")
	copyFolder := filepath.Join(root, "archive", "hen", "journal-copy")
	other := filepath.Join(folder, "dana", "friend", "dana")
	for _, dir := range []string{other, copyFolder, filepath.Join(folder, "hen")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "file"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := peers.OpenRegistry(filepath.Join(root, "peers.json"))
	if err != nil {
		t.Fatal(err)
	}
	host := peers.NewHost(folder, registry, nil, time.Now)
	s := &swapService{host: host, now: time.Now, offers: map[string]swapOffer{}}
	for _, name := range []string{"..", "../..", "../dana", `..\dana`, "../dana/friend", "/", ".", "", `C:\`, "a/b"} {
		if err := s.Revoke("hen", name, true); err == nil {
			t.Errorf("revokeSwap(%q, deleteFiles: true) was taken", name)
		}
		if err := host.Remove("hen", name); err == nil {
			t.Errorf("Host.Remove(hen, %q) was taken", name)
		}
		if err := s.Remove("hen", name); err == nil {
			t.Errorf("removeSwap(%q) was taken", name)
		}
	}
	if err := host.Remove("../..", "x"); err == nil {
		t.Error("Host.Remove took an owner that is a path")
	}
	for _, dir := range []string{other, copyFolder, filepath.Join(folder, "hen")} {
		if _, err := os.Stat(filepath.Join(dir, "file")); err != nil {
			t.Errorf("%s is gone: %v", dir, err)
		}
	}
}

// A device kept from private files learns nothing of them: not their sha256 from a cursor it wrote itself, and not
// their name, facts, note, text or faces from startUpload.
func TestADeviceKeptFromPrivateFilesLearnsNothingOfThem(t *testing.T) {
	env := newTestEnv(t)
	phone := env.enrolledClient(t)
	tablet := env.enrolledClient(t)
	devices := env.enrollment.Registry.List("hen")
	if _, err := env.enrollment.Registry.SetWithholdPrivate(devices[1].Serial, true); err != nil {
		t.Fatal(err)
	}
	shown := env.uploadPhoto(t, phone, []byte("a beach"), "BEACH.jpg")
	private := env.uploadPhoto(t, phone, []byte("a bedroom"), "BEDROOM.jpg")
	env.graphql(t, phone, `mutation($h: [Sha256!]!) { setPrivate(sha256s: $h, private: true) { sha256 } }`, map[string]any{"h": []string{private}})
	note := env.graphql(t, phone, `mutation($s: Sha256!) { setNote(sha256: $s, note: "{\"title\":\"a secret\"}") { __typename } }`, map[string]any{"s": private})
	if !strings.Contains(string(note.Data["setNote"]), "NoteSaved") {
		t.Fatalf("setNote = %s %+v", note.Data["setNote"], note.Errors)
	}

	// Cursors the tablet wrote itself: one the server can't read, and ones that claim it was never kept from them.
	claimed, _ := json.Marshal(map[string]any{"v": 3, "h": map[string]any{}, "t": 1, "s": strings.Repeat("0", 64)})
	forged := []string{"x", base64.RawURLEncoding.EncodeToString(claimed),
		base64.RawURLEncoding.EncodeToString([]byte("m1:1:" + strings.Repeat("0", 64)))}
	for _, cursor := range forged {
		resp := env.graphql(t, tablet, `query($s: String) { changes(since: $s) { changes { path deleted } } }`, map[string]any{"s": cursor})
		got := string(resp.Data["changes"])
		if strings.Contains(got, private) || !strings.Contains(got, shown) {
			t.Errorf("the tablet's feed from the cursor %q: %s %+v", cursor, got, resp.Errors)
		}
	}
	// A cursor the server gave it is signed and goes on from where it was; the signature of one cursor doesn't
	// pass for another.
	resp := env.graphql(t, tablet, `query { changes { cursor changes { path } } }`, nil)
	var page struct{ Cursor string }
	json.Unmarshal(resp.Data["changes"], &page)
	_, mac, signed := strings.Cut(page.Cursor, ".")
	if !signed || mac == "" {
		t.Fatalf("the tablet's cursor isn't signed: %q", page.Cursor)
	}
	resp = env.graphql(t, tablet, `query($s: String) { changes(since: $s) { changes { path } } }`, map[string]any{"s": page.Cursor})
	if got := string(resp.Data["changes"]); strings.Contains(got, shown) {
		t.Errorf("the tablet's feed from its own cursor started again: %s", got)
	}
	resp = env.graphql(t, tablet, `query($s: String) { changes(since: $s) { changes { path deleted } } }`,
		map[string]any{"s": base64.RawURLEncoding.EncodeToString(claimed) + "." + mac})
	if got := string(resp.Data["changes"]); strings.Contains(got, private) || !strings.Contains(got, shown) {
		t.Errorf("a cursor with another's signature was taken: %s", got)
	}

	// startUpload says the server has the file, and nothing else about it.
	start := `mutation($s: Sha256!) { startUpload(input: {sha256: $s, size: 9, fileName: "mine.jpg", mimeType: "image/jpeg", kind: PHOTO}) {
		__typename ... on MediaExists { media { sha256 fileName size private uploadedBy takenAt note text { text } faces { id } aesthetic } } } }`
	resp = env.graphql(t, tablet, start, map[string]any{"s": private})
	got := string(resp.Data["startUpload"])
	if !strings.Contains(got, `"MediaExists"`) || !strings.Contains(got, `"fileName":"mine.jpg"`) || !strings.Contains(got, `"note":null`) ||
		strings.Contains(got, "BEDROOM") || strings.Contains(got, "secret") || strings.Contains(got, `"private":true`) {
		t.Errorf("the tablet's startUpload of a private file: %s %+v", got, resp.Errors)
	}
	// The phone, which may see it, gets it whole.
	resp = env.graphql(t, phone, start, map[string]any{"s": private})
	if got := string(resp.Data["startUpload"]); !strings.Contains(got, "BEDROOM.jpg") || !strings.Contains(got, "a secret") {
		t.Errorf("the phone's startUpload: %s %+v", got, resp.Errors)
	}
}

// With "enrolled_only", a certificate the CA signed but the registry doesn't list is refused.
func TestOnlyEnrolledCertificatesWhenSet(t *testing.T) {
	for _, only := range []bool{false, true} {
		env := newTestEnv(t, func(c *auth.AppConfig) { c.EnrolledOnly = only })
		enrolled := env.enrolledClient(t)
		if resp := env.graphql(t, enrolled, `{ serverInfo { api } }`, nil); resp.Status != 200 || len(resp.Errors) != 0 {
			t.Fatalf("enrolled_only %v: an enrolled device: %+v", only, resp.Errors)
		}
		// Signed by the CA, never enrolled: as one made with scripts/generate-user-cert.ps1.
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		der, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
		csr, _ := x509.ParseCertificateRequest(der)
		cert, err := env.enrollment.Issuer.Issue(csr, "hen")
		if err != nil {
			t.Fatal(err)
		}
		unlisted := env.client(&tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key})
		resp, err := unlisted.Post(env.server.URL+"/query", "application/json", strings.NewReader(`{"query":"{ serverInfo { api } }"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if want := map[bool]int{false: 200, true: 401}[only]; resp.StatusCode != want {
			t.Errorf("enrolled_only %v: a certificate the registry doesn't list got %d, want %d", only, resp.StatusCode, want)
		}
	}
}

func TestSwapLinksNeverSendTheServerToItself(t *testing.T) {
	for address, want := range map[string]bool{
		"127.0.0.1:56398": false, "[::1]:56398": false, "localhost:56398": false, "a.localhost:1": false,
		"0.0.0.0:56398": false, "169.254.169.254:80": false, "[fe80::1]:56398": false, "224.0.0.1:1": false,
		"[::ffff:127.0.0.1]:56398": false,
		"192.168.1.20:56398":       true, "203.0.113.7:56398": true, "friend.example.com:56398": true,
		"[2001:db8::1]:56398": true,
	} {
		if got := dialable(address); got != want {
			t.Errorf("dialable(%s) = %v, want %v", address, got, want)
		}
	}
}
