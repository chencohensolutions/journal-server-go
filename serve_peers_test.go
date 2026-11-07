package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/copies"
	"github.com/journal/server-go/media"
	"github.com/journal/server-go/peers"
)

// A swap's one direction: dana's server invites hen's (peer invite), hen's adds it (peer add), and hen's copy goes
// there; hen's phone sees it as a copy on a peer. dana's server refuses the peer certificate anywhere but /peer/.
func TestACopyOnAFriendsServer(t *testing.T) {
	dana := newTestEnv(t, func(c *auth.AppConfig) {
		c.PeersFolder = t.TempDir()
		c.PeersFile = filepath.Join(t.TempDir(), "peers.json")
	})
	if dana.peers == nil {
		t.Fatal("no peer host with peers_folder")
	}

	// dana's admin invites hen's server through the admin API, as peer invite does.
	admin := httptest.NewServer(newAdminMux(dana.config, dana.enrollment, "secret", dana.refreshIndexes, nil, nil, dana.peers))
	defer admin.Close()
	tokenFile := filepath.Join(t.TempDir(), "admin.token")
	os.WriteFile(tokenFile, []byte("secret\n"), 0o600)
	danaAdmin := dana.config
	danaAdmin.AdminAddr, danaAdmin.AdminTokenFile = strings.TrimPrefix(admin.URL, "http://"), tokenFile
	adminCall := func(query string, out any) {
		t.Helper()
		if err := adminQuery(danaAdmin, query, nil, out); err != nil {
			t.Fatal(err)
		}
	}
	var invite struct {
		CreatePeerInvite struct{ Code string } `json:"createPeerInvite"`
	}
	adminCall(`mutation { createPeerInvite(name: "hen-home", quota: 1e9) { code } }`, &invite)
	code := invite.CreatePeerInvite.Code
	if code == "" {
		t.Fatal("no invite code")
	}

	// hen's server adds dana with the code and dana's fingerprint, then keeps hen's copy there.
	henConfig := auth.AppConfig{PeersDir: filepath.Join(t.TempDir(), "peers")}.WithDefaults()
	fingerprint := peers.Fingerprint(dana.server.Certificate())
	if err := peerAddCommand(henConfig, []string{"dana", dana.server.URL, code + "." + strings.Repeat("0", 64), "--trusted"}); err == nil {
		t.Fatal("added with a wrong fingerprint")
	}
	if err := peerAddCommand(henConfig, []string{"dana", dana.server.URL, code + "." + fingerprint, "--trusted"}); err != nil {
		t.Fatal(err)
	}
	if err := peerAddCommand(henConfig, []string{"dana2", dana.server.URL, code + "." + fingerprint}); err == nil {
		t.Fatal("an invite worked twice")
	}
	hen := newTestEnv(t, func(c *auth.AppConfig) {
		c.PeersDir = henConfig.PeersDir
		c.Users[0].Copies = []auth.CopyConfig{{Peer: "dana", At: "02:00"}}
	})
	client := hen.enrolledClient(t)
	hen.uploadPhoto(t, client, []byte("a photo for dana's server"), "IMG_1.jpg")
	runCopy(context.Background(), hen.copiers[0])
	var found bool
	filepath.WalkDir(filepath.Join(dana.config.PeersFolder, "hen-home", "hen", "media"), func(path string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == "IMG_1.jpg" {
			data, _ := os.ReadFile(path)
			found = string(data) == "a photo for dana's server"
		}
		return nil
	})
	if !found {
		t.Fatal("the photo isn't at dana's server")
	}

	// hen's phone sees the copy on a peer.
	resp := hen.graphql(t, client, `{ serverInfo { peerCopies } copies { folder kind encrypted copied missing freeBytes problem } }`, nil)
	var list []struct {
		Folder, Kind    string
		Encrypted       bool
		Copied, Missing int
		FreeBytes       *int64
		Problem         *string
	}
	if len(resp.Errors) > 0 || json.Unmarshal(resp.Data["copies"], &list) != nil || len(list) != 1 {
		t.Fatalf("copies: %+v", resp)
	}
	if c := list[0]; c.Folder != "dana" || c.Kind != "PEER" || c.Encrypted || c.Copied != 1 || c.Missing != 0 ||
		c.FreeBytes == nil || *c.FreeBytes <= 0 || c.Problem != nil {
		t.Errorf("the copy on dana's server: %+v", c)
	}

	// The peer's certificate reaches dana's peer API, never a user's routes; and dana's admin sees it.
	info, cert, err := loadPeer(henConfig, "dana")
	if err != nil || info.HostName != "hen-home" || !info.Trusted {
		t.Fatalf("hen's record of dana: %+v %v", info, err)
	}
	peerClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: dana.roots,
		Certificates: []tls.Certificate{cert}}}}
	for _, path := range []string{"/query", "/media/" + strings.Repeat("a", 64), "/index/people"} {
		r, err := peerClient.Get(dana.server.URL + path)
		if err != nil || r.StatusCode != http.StatusUnauthorized {
			t.Errorf("a peer certificate on %s: %v %v", path, r, err)
		}
	}
	var listed struct {
		Peers []struct {
			Name string
			Used float64
		}
	}
	adminCall(`{ peers { name used } }`, &listed)
	if len(listed.Peers) != 1 || listed.Peers[0].Name != "hen-home" || listed.Peers[0].Used == 0 {
		t.Errorf("dana's peers: %+v", listed.Peers)
	}

	// Revoked: hen's next pass stops, and the phone is told why.
	adminCall(`mutation { revokePeer(name: "hen-home") { name } }`, &struct{}{})
	hen.uploadPhoto(t, client, []byte("one more"), "IMG_2.jpg")
	runCopy(context.Background(), hen.copiers[0])
	resp = hen.graphql(t, client, `{ copies { problem problemSince } }`, nil)
	if !strings.Contains(string(resp.Data["copies"]), `"PEER_UNREACHABLE"`) {
		t.Errorf("after the peer was revoked: %s", resp.Data["copies"])
	}
}

// A plain copy needs a trusted peer; a missing peer stops the server at start.
func TestCopiesToPeersAreChecked(t *testing.T) {
	config := auth.AppConfig{PeersDir: t.TempDir(), Users: []auth.UserConfig{{Username: "hen", DataFolder: t.TempDir(),
		Copies: []auth.CopyConfig{{Peer: "dana"}}}}}.WithDefaults()
	library, err := openLibrary(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newCopiers(config, library); err == nil || !strings.Contains(err.Error(), "peer add") {
		t.Errorf("a copy to a peer not added: %v", err)
	}
	for _, bad := range [][]auth.CopyConfig{
		{{Peer: "dana", Folder: "E:\\copy"}},
		{{Peer: "dana"}, {Peer: "dana", At: "04:00"}},
		{{Folder: t.TempDir(), Encrypt: true}},
	} {
		u := auth.UserConfig{Username: "hen", DataFolder: t.TempDir(), Copies: bad}
		cfg := auth.AppConfig{Users: []auth.UserConfig{u}}.WithDefaults()
		path := filepath.Join(t.TempDir(), "config.json")
		data, _ := json.Marshal(cfg)
		os.WriteFile(path, data, 0o644)
		if _, err := auth.LoadConfig(path); err == nil {
			t.Errorf("config with %+v loaded", bad)
		}
	}
}

// An encrypted copy goes to a peer that isn't trusted: copy-key new makes the key (the owner types back the end of
// the key file), and the phone sees the copy as encrypted.
func TestAnEncryptedCopyOnAFriendsServer(t *testing.T) {
	dana := newTestEnv(t, func(c *auth.AppConfig) {
		c.PeersFolder = t.TempDir()
		c.PeersFile = filepath.Join(t.TempDir(), "peers.json")
	})
	code, _, err := dana.peers.Invite("", "hen-home", 1e9)
	if err != nil {
		t.Fatal(err)
	}
	henConfig := auth.AppConfig{PeersDir: filepath.Join(t.TempDir(), "peers"), CopyKeysDir: filepath.Join(t.TempDir(), "keys"),
		Users: []auth.UserConfig{{Username: "hen"}}}.WithDefaults()
	if err := peerAddCommand(henConfig, []string{"dana", dana.server.URL, code + "." + peers.Fingerprint(dana.server.Certificate())}); err != nil {
		t.Fatal(err)
	}

	keyFile := filepath.Join(t.TempDir(), "hen.journalkey")
	var out strings.Builder
	if err := copyKeyCommand(henConfig, []string{"new", "hen", "--out", keyFile}, strings.NewReader("wrong!\n"), &out); err == nil {
		t.Fatal("made with the wrong end typed")
	}
	if _, err := os.Stat(keyFile); err == nil {
		t.Fatal("the key file stayed after the wrong end was typed")
	}
	// The end is only known once the file is written: type it from a reader that reads the file when asked.
	typed := readerFunc(func() string {
		data, _ := os.ReadFile(keyFile)
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		last := lines[len(lines)-1]
		return last[len(last)-6:] + "\n"
	})
	if err := copyKeyCommand(henConfig, []string{"new", "hen", "--out", keyFile}, typed, &out); err != nil {
		t.Fatal(err)
	}
	if err := copyKeyCommand(henConfig, []string{"new", "hen", "--out", keyFile + "2"}, typed, &out); err == nil {
		t.Fatal("a second copy key was made")
	}
	data, _ := os.ReadFile(keyFile)
	if !strings.Contains(string(data), "AGE-SECRET-KEY-1") || !strings.Contains(string(data), "# journal-naming-key: ") {
		t.Fatalf("the key file: %s", data)
	}
	stored, _ := os.ReadFile(copyKeyFile(henConfig, "hen"))
	if strings.Contains(string(stored), "AGE-SECRET-KEY") {
		t.Fatal("the server kept the private key")
	}

	// Plain to an untrusted peer is refused; encrypted goes.
	hen := newTestEnv(t, func(c *auth.AppConfig) {
		c.PeersDir, c.CopyKeysDir = henConfig.PeersDir, henConfig.CopyKeysDir
		c.Users[0].Copies = []auth.CopyConfig{{Peer: "dana", Encrypt: true}}
	})
	if _, err := newCopiers(func() auth.AppConfig {
		c := hen.config
		c.Users = []auth.UserConfig{{Username: "hen", DataFolder: hen.dataFolder, Copies: []auth.CopyConfig{{Peer: "dana"}}}}
		return c
	}(), hen.library); err == nil || !strings.Contains(err.Error(), "trusted") {
		t.Errorf("a plain copy to an untrusted peer: %v", err)
	}
	client := hen.enrolledClient(t)
	hen.uploadPhoto(t, client, []byte("a photo dana mustn't see"), "IMG_1.jpg")
	runCopy(context.Background(), hen.copiers[0])
	filepath.WalkDir(dana.config.PeersFolder, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if data, _ := os.ReadFile(path); strings.Contains(string(data), "mustn't") || strings.Contains(path, "IMG_1") {
				t.Errorf("dana can read %s", path)
			}
		}
		return nil
	})
	resp := hen.graphql(t, client, `{ copies { kind encrypted copied missing } }`, nil)
	if !strings.Contains(string(resp.Data["copies"]), `"encrypted":true`) || !strings.Contains(string(resp.Data["copies"]), `"copied":1`) {
		t.Errorf("the phone's view: %s %+v", resp.Data["copies"], resp.Errors)
	}
}

// readerFunc gives next's line on each read.
type readerFunc func() string

func (f readerFunc) Read(b []byte) (int, error) { return copy(b, f()), nil }

// The owner's server is lost with everything on it but the key file (sealed with a passphrase). dana revokes the old
// peer and invites the new server by the same name; the new server adds dana and restores hen's files.
func TestAServerLostIsRestoredFromAFriend(t *testing.T) {
	dana := newTestEnv(t, func(c *auth.AppConfig) {
		c.PeersFolder = t.TempDir()
		c.PeersFile = filepath.Join(t.TempDir(), "peers.json")
	})
	fingerprint := peers.Fingerprint(dana.server.Certificate())
	keyFile := filepath.Join(t.TempDir(), "hen.journalkey")

	// Before: hen's server keeps an encrypted copy at dana's.
	code, _, _ := dana.peers.Invite("", "hen-home", 1e9)
	old := auth.AppConfig{PeersDir: filepath.Join(t.TempDir(), "peers"), CopyKeysDir: filepath.Join(t.TempDir(), "keys"),
		Users: []auth.UserConfig{{Username: "hen"}}}.WithDefaults()
	if err := peerAddCommand(old, []string{"dana", dana.server.URL, code + "." + fingerprint}); err != nil {
		t.Fatal(err)
	}
	const passphrase = "correct horse battery staple"
	answers := strings.NewReader(passphrase + "\n" + passphrase + "\n")
	typed := readerFunc(func() string {
		if answers.Len() > 0 {
			line, _ := bufioLine(answers)
			return line
		}
		return keyEnd(t, keyFile, passphrase) + "\n"
	})
	var out strings.Builder
	if err := copyKeyCommand(old, []string{"new", "hen", "--out", keyFile, "--passphrase"}, typed, &out); err != nil {
		t.Fatal(err, out.String())
	}
	hen := newTestEnv(t, func(c *auth.AppConfig) {
		c.PeersDir, c.CopyKeysDir = old.PeersDir, old.CopyKeysDir
		c.Users[0].Copies = []auth.CopyConfig{{Peer: "dana", Encrypt: true}}
	})
	client := hen.enrolledClient(t)
	hen.uploadPhoto(t, client, []byte("the first photo"), "IMG_1.jpg")
	hen.uploadPhoto(t, client, []byte("the second photo"), "IMG_2.jpg")
	runCopy(context.Background(), hen.copiers[0])

	// After: a new server, with nothing of the old one. dana lets it in under the old name.
	if _, err := dana.peers.Registry.Revoke("hen-home", time.Now()); err != nil {
		t.Fatal(err)
	}
	code, _, _ = dana.peers.Invite("", "hen-home", 1e9)
	fresh := auth.AppConfig{PeersDir: filepath.Join(t.TempDir(), "peers"), CopyKeysDir: filepath.Join(t.TempDir(), "keys"),
		Users: []auth.UserConfig{{Username: "hen"}}}.WithDefaults()
	if err := peerAddCommand(fresh, []string{"dana", dana.server.URL, code + "." + fingerprint}); err != nil {
		t.Fatal(err)
	}
	to := filepath.Join(t.TempDir(), "hen")
	out.Reset()
	err := restoreCommand(fresh, []string{"hen", "--from", "dana", "--to", to, "--key", keyFile}, strings.NewReader(passphrase+"\n"), &out)
	if err != nil {
		t.Fatal(err, out.String())
	}
	sameFiles := 0
	filepath.WalkDir(filepath.Join(hen.dataFolder, "media"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(hen.dataFolder, path)
		want, _ := os.ReadFile(path)
		got, err := os.ReadFile(filepath.Join(to, rel))
		if err != nil || string(got) != string(want) {
			t.Errorf("%s came back as %q, %v", rel, got, err)
		} else {
			sameFiles++
		}
		return nil
	})
	if sameFiles < 4 || !strings.Contains(out.String(), "Every original matched") {
		t.Errorf("%d files back: %s", sameFiles, out.String())
	}
	// The restored folder opens as a data folder, with both photos.
	cfg := auth.AppConfig{Users: []auth.UserConfig{{Username: "hen", DataFolder: to}}}.WithDefaults()
	library, err := openLibrary(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if u := library.Store("hen").Usage(media.View{}); u.Photos != 2 {
		t.Errorf("the restored folder holds %+v", u)
	}
	// Without --to, nothing is restored anywhere.
	if err := restoreCommand(fresh, []string{"hen", "--from", "dana", "--key", keyFile}, strings.NewReader(""), &out); err == nil {
		t.Error("a restore without --to")
	}
}

// keyEnd is the last 6 characters of the key in a key file sealed with passphrase.
func keyEnd(t *testing.T, keyFile, passphrase string) string {
	t.Helper()
	data, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	kf, err := copies.ReadKeyFile(data, func() (string, error) { return passphrase, nil })
	if err != nil {
		t.Fatal(err)
	}
	s := fmt.Sprint(kf.Identity)
	return s[len(s)-6:]
}

func bufioLine(r *strings.Reader) (string, error) {
	var line []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return string(line), err
		}
		line = append(line, b)
		if b == '\n' {
			return string(line), nil
		}
	}
}

// A copy key from a passphrase: no key file; the passphrase alone restores, and export writes a key file of it.
func TestACopyKeyFromAPassphrase(t *testing.T) {
	defer func(d copies.Derivation) { copies.DefaultDerivation = d }(copies.DefaultDerivation)
	copies.DefaultDerivation = copies.Derivation{Time: 1, MemoryKiB: 1024, Threads: 1}
	dana := newTestEnv(t, func(c *auth.AppConfig) {
		c.PeersFolder = t.TempDir()
		c.PeersFile = filepath.Join(t.TempDir(), "peers.json")
	})
	code, _, _ := dana.peers.Invite("", "hen-home", 1e9)
	config := auth.AppConfig{PeersDir: filepath.Join(t.TempDir(), "peers"), CopyKeysDir: filepath.Join(t.TempDir(), "keys"),
		Users: []auth.UserConfig{{Username: "hen"}}}.WithDefaults()
	if err := peerAddCommand(config, []string{"dana", dana.server.URL, code + "." + peers.Fingerprint(dana.server.Certificate())}); err != nil {
		t.Fatal(err)
	}
	const passphrase = "six random words make a passphrase"
	var out strings.Builder
	if err := copyKeyCommand(config, []string{"new", "hen", "--from-passphrase"}, strings.NewReader("too short\n"), &out); err == nil {
		t.Fatal("a weak passphrase was taken")
	}
	if err := copyKeyCommand(config, []string{"new", "hen", "--from-passphrase"}, strings.NewReader(passphrase+"\n"+passphrase+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	if stored, _ := os.ReadFile(copyKeyFile(config, "hen")); strings.Contains(string(stored), passphrase) || !strings.Contains(string(stored), `"derived"`) {
		t.Fatalf("what the server keeps: %s", stored)
	}
	hen := newTestEnv(t, func(c *auth.AppConfig) {
		c.PeersDir, c.CopyKeysDir = config.PeersDir, config.CopyKeysDir
		c.Users[0].Copies = []auth.CopyConfig{{Peer: "dana", Encrypt: true}}
	})
	hen.uploadPhoto(t, hen.enrolledClient(t), []byte("a photo"), "IMG_1.jpg")
	runCopy(context.Background(), hen.copiers[0])

	to := filepath.Join(t.TempDir(), "hen")
	if err := restoreCommand(config, []string{"hen", "--from", "dana", "--to", to, "--passphrase"}, strings.NewReader("a wrong passphrase, sadly\n"), &out); err == nil {
		t.Error("restored with a wrong passphrase")
	}
	os.RemoveAll(to)
	if err := restoreCommand(config, []string{"hen", "--from", "dana", "--to", to, "--passphrase"}, strings.NewReader(passphrase+"\n"), &out); err != nil {
		t.Fatal(err, out.String())
	}
	keyFile := filepath.Join(t.TempDir(), "hen.journalkey")
	if err := copyKeyCommand(config, []string{"export", "hen", "--out", keyFile}, strings.NewReader("not it at all, no\n"), &out); err == nil {
		t.Error("exported with a wrong passphrase")
	}
	if err := copyKeyCommand(config, []string{"export", "hen", "--out", keyFile}, strings.NewReader(passphrase+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	to2 := filepath.Join(t.TempDir(), "hen2")
	if err := restoreCommand(config, []string{"hen", "--from", "dana", "--to", to2, "--key", keyFile}, strings.NewReader(""), &out); err != nil {
		t.Fatal("the exported key file: ", err)
	}
}

// A peer certificate near its end is renewed by the owner's server: the copies go on with the new one at once, and
// the host refuses the old one from then on.
func TestAPeerCertificateIsRenewed(t *testing.T) {
	dana := newTestEnv(t, func(c *auth.AppConfig) {
		c.PeersFolder = t.TempDir()
		c.PeersFile = filepath.Join(t.TempDir(), "peers.json")
	})
	code, _, _ := dana.peers.Invite("", "hen-home", 1e9)
	config := auth.AppConfig{PeersDir: filepath.Join(t.TempDir(), "peers"), Users: []auth.UserConfig{{Username: "hen"}}}.WithDefaults()
	if err := peerAddCommand(config, []string{"dana", dana.server.URL, code + "." + peers.Fingerprint(dana.server.Certificate()), "--trusted"}); err != nil {
		t.Fatal(err)
	}
	hen := newTestEnv(t, func(c *auth.AppConfig) {
		c.PeersDir = config.PeersDir
		c.Users[0].Copies = []auth.CopyConfig{{Peer: "dana"}}
	})
	_, oldCert, _ := loadPeer(config, "dana")
	oldInfo, _, _ := loadPeer(config, "dana")

	// Far from its end: nothing happens.
	renewPeers(config, hen.copiers, oldInfo.ExpiresAt.Add(-40*24*time.Hour))
	if _, cert, _ := loadPeer(config, "dana"); string(cert.Certificate[0]) != string(oldCert.Certificate[0]) {
		t.Fatal("renewed 40 days before the end")
	}
	// Within 30 days of it: renewed, and the copy uses the new certificate.
	renewPeers(config, hen.copiers, oldInfo.ExpiresAt.Add(-10*24*time.Hour))
	_, newCert, err := loadPeer(config, "dana")
	if err != nil || string(newCert.Certificate[0]) == string(oldCert.Certificate[0]) {
		t.Fatalf("not renewed: %v", err)
	}
	hen.uploadPhoto(t, hen.enrolledClient(t), []byte("a photo"), "IMG_1.jpg")
	runCopy(context.Background(), hen.copiers[0])
	if _, last := hen.copiers[0].Status(); last == nil || last.Err != nil || last.Files == 0 {
		t.Fatalf("the copy after renewing: %+v", last)
	}
	// The old certificate is refused now that the new one was used.
	old := copies.NewPeer("dana", dana.server.URL, "hen", oldCert, peers.Fingerprint(dana.server.Certificate()))
	if err := old.Ready(); err == nil {
		t.Error("the old certificate still works")
	}
	if p, _ := dana.peers.Registry.Active("hen-home"); p.PreviousSerial != "" {
		t.Errorf("dana still accepts the old certificate: %+v", p)
	}
}
