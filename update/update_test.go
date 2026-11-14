package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// release serves a made-up latest release: SHA256SUMS for version, signed by key, and program as linux-amd64's.
type release struct {
	version string
	program []byte
	key     ed25519.PrivateKey
	sums    string // instead of the one made from program, when set
	sig     []byte // instead of the signature by key, when set
}

func (r release) serve(t *testing.T) *httptest.Server {
	t.Helper()
	name := AssetName("linux", "amd64")
	hash := sha256.Sum256(r.program)
	sums := r.sums
	if sums == "" {
		sums = "# journal-server " + r.version + "\n" + hex.EncodeToString(hash[:]) + "  " + name + "\n"
	}
	sig := r.sig
	if sig == nil {
		sig = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(r.key, []byte(sums))))
	}
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/repos/owner/repo/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		type asset struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v" + r.version, "html_url": srv.URL + "/release",
			"assets": []asset{{Sums, srv.URL + "/sums"}, {Signature, srv.URL + "/sig"}, {name, srv.URL + "/program"}},
		})
	})
	mux.HandleFunc("/sums", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(sums)) })
	mux.HandleFunc("/sig", func(w http.ResponseWriter, _ *http.Request) { w.Write(sig) })
	mux.HandleFunc("/program", func(w http.ResponseWriter, _ *http.Request) { w.Write(r.program) })
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func updater(srv *httptest.Server, current string, keys ...ed25519.PublicKey) *Updater {
	return &Updater{API: srv.URL, Repo: "owner/repo", Keys: keys, Client: srv.Client(), Current: current,
		GOOS: "linux", GOARCH: "amd64"}
}

// fakeProgram is a shell script that answers `version` as journal-server ver does.
func fakeProgram(t *testing.T, ver string) []byte {
	if runtime.GOOS == "windows" {
		t.Skip("the made-up program is a shell script")
	}
	return []byte("#!/bin/sh\necho 'journal-server " + ver + " (abc1234 2026-11-08)'\n")
}

func TestANewerSignedReleaseIsInstalledAndCanBeRolledBack(t *testing.T) {
	pub, priv := newKey(t)
	srv := release{version: "1.3.0", program: fakeProgram(t, "1.3.0"), key: priv}.serve(t)
	u := updater(srv, "1.2.9", pub)

	r, err := u.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if newer, major := u.Newer(r); !newer || major {
		t.Fatalf("Newer = %v, %v; want a newer release of the same major version", newer, major)
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "journal-server")
	os.WriteFile(exe, []byte("the running program"), 0o755)
	downloaded, err := u.Download(context.Background(), r, exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := Install(exe, downloaded); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(exe); !strings.Contains(string(got), "1.3.0") {
		t.Errorf("the program is %q, want the release's", got)
	}
	if got, _ := os.ReadFile(exe + ".old"); string(got) != "the running program" {
		t.Errorf(".old is %q, want the program it replaced", got)
	}

	if err := Rollback(exe); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "the running program" {
		t.Errorf("after a rollback the program is %q", got)
	}
	if got, _ := os.ReadFile(exe + ".old"); !strings.Contains(string(got), "1.3.0") {
		t.Errorf("after a rollback .old is %q, want the release (so a second rollback undoes the first)", got)
	}
}

func TestAReleaseNotSignedByTheReleaseKeyIsRefused(t *testing.T) {
	pub, _ := newKey(t)
	_, other := newKey(t)
	srv := release{version: "1.3.0", program: []byte("x"), key: other}.serve(t)
	if _, err := updater(srv, "1.2.0", pub).Latest(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "isn't signed") {
		t.Fatalf("Latest = %v, want a refusal", err)
	}
	if _, err := updater(srv, "1.2.0").Latest(context.Background()); err == nil {
		t.Fatal("a build without a release key accepted a release")
	}
}

func TestAProgramWhoseSha256DiffersIsRefused(t *testing.T) {
	pub, priv := newKey(t)
	sums := "# journal-server 1.3.0\n" + strings.Repeat("0", 64) + "  " + AssetName("linux", "amd64") + "\n"
	srv := release{version: "1.3.0", program: []byte("tampered"), key: priv, sums: sums}.serve(t)
	u := updater(srv, "1.2.0", pub)
	r, err := u.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), "journal-server")
	if _, err := u.Download(context.Background(), r, exe); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("Download = %v, want a refusal", err)
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Error("the refused download was left behind")
	}
}

func TestAProgramThatIsntTheSignedVersionIsRefused(t *testing.T) {
	pub, priv := newKey(t)
	// An older program, published again with a signature over its own SHA256SUMS but a newer tag.
	srv := release{version: "1.3.0", program: fakeProgram(t, "1.1.0"), key: priv}.serve(t)
	u := updater(srv, "1.2.0", pub)
	r, err := u.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Download(context.Background(), r, filepath.Join(t.TempDir(), "journal-server")); err == nil {
		t.Fatal("a program that says it is 1.1.0 was accepted as 1.3.0")
	}
}

func TestVersionsAreComparedByNumber(t *testing.T) {
	u := &Updater{Current: "1.9.3"}
	for _, c := range []struct {
		version      string
		newer, major bool
	}{
		{"1.10.0", true, false},
		{"1.9.4", true, false},
		{"1.9.3", false, false},
		{"1.9.2", false, false},
		{"2.0.0", true, true},
		{"0.9.9", false, false},
		{"junk", false, false},
	} {
		newer, major := u.Newer(&Release{Version: c.version})
		if newer != c.newer || major != c.major {
			t.Errorf("Newer(%s) = %v, %v; want %v, %v", c.version, newer, major, c.newer, c.major)
		}
	}
}

func TestDateVersions(t *testing.T) {
	cases := []struct {
		current, version string
		newer, major     bool
	}{
		{"1.2.8", "2026.11.14", true, false},
		{"2026.11.14", "2026.11.14.2", true, false},
		{"2026.11.14.2", "2026.11.15", true, false},
		{"2026.12.31", "2027.1.2", true, false},
		{"2026.11.14.2", "2026.11.14", false, false},
		{"2026.11.14", "1.9.0", false, false},
	}
	for _, c := range cases {
		newer, major := (&Updater{Current: c.current}).Newer(&Release{Version: c.version})
		if newer != c.newer || major != c.major {
			t.Errorf("%s: Newer(%s) = %v, %v; want %v, %v", c.current, c.version, newer, major, c.newer, c.major)
		}
	}
}

func TestSumsMustNameTheVersion(t *testing.T) {
	if _, _, err := ParseSums([]byte(strings.Repeat("a", 64) + "  journal-server-linux-amd64\n")); err == nil {
		t.Error("SHA256SUMS without its version line was read")
	}
}

func TestTheCompiledInKeysAreReadable(t *testing.T) {
	if _, err := ParseKeys(keysFile); err != nil {
		t.Fatal(err)
	}
}
