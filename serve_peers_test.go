package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/journal/server-go/auth"
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
		{{Peer: "dana", Encrypt: true}},
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
