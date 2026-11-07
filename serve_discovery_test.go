package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/discovery"
	"github.com/journal/server-go/peers"
)

// forward is a home line in front of srv: an address that reaches it until closed (the line's IP changed).
func forward(t *testing.T, srv *httptest.Server) (addr string, closeLine func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := strings.TrimPrefix(srv.URL, "https://")
	var mu sync.Mutex
	var open []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s, err := net.Dial("tcp", target)
			if err != nil {
				c.Close()
				continue
			}
			mu.Lock()
			open = append(open, c, s)
			mu.Unlock()
			go func() { io.Copy(s, c); s.Close() }()
			go func() { io.Copy(c, s); c.Close() }()
		}
	}()
	return ln.Addr().String(), func() {
		ln.Close()
		mu.Lock()
		for _, c := range open {
			c.Close()
		}
		mu.Unlock()
	}
}

// dana's home IP changes after hen's server added it: hen's copies follow, first by what dana told it, then by the
// DHT when nothing it knows answers.
func TestAPeerIsFoundAfterItsAddressChanges(t *testing.T) {
	dana := newTestEnv(t, func(c *auth.AppConfig) {
		c.PeersFolder = t.TempDir()
		c.PeersFile = filepath.Join(t.TempDir(), "peers.json")
	})
	fingerprint := peers.Fingerprint(dana.server.Certificate())
	dhtKey, _ := discovery.KeyFor(dana.server.TLS.Certificates[0].PrivateKey)
	keyHex := hex.EncodeToString(dhtKey.Public().(ed25519.PublicKey))
	var mu sync.Mutex
	danaSays := []string{"https://127.0.0.1:1"} // an address that answers nothing
	dana.peers.Discovery = func() peers.Discovery {
		mu.Lock()
		defer mu.Unlock()
		return peers.Discovery{Addresses: danaSays, Key: keyHex}
	}

	first, closeFirst := forward(t, dana.server)
	code := danaInvite(t, dana)
	henConfig := auth.AppConfig{PeersDir: filepath.Join(t.TempDir(), "peers")}.WithDefaults()
	// The invite named by the server id rather than the fingerprint.
	if err := peerAddCommand(henConfig, []string{"dana", "https://" + first, code + "." + discovery.FormatID(fingerprint), "--trusted"}); err != nil {
		t.Fatal(err)
	}
	info, _, err := loadPeer(henConfig, "dana")
	if err != nil || info.DiscoveryKey != keyHex || len(info.Addresses) != 2 || info.Addresses[0] != first {
		t.Fatalf("hen's record of dana: %+v %v", info, err)
	}

	hen := newTestEnv(t, func(c *auth.AppConfig) {
		c.PeersDir = henConfig.PeersDir
		c.Users[0].Copies = []auth.CopyConfig{{Peer: "dana", At: "02:00"}}
	})
	client := hen.enrolledClient(t)
	copied := func(name string) bool {
		found := false
		filepath.WalkDir(filepath.Join(dana.config.PeersFolder, "hen-home", "hen", "media"), func(path string, d os.DirEntry, err error) error {
			found = found || (err == nil && d.Name() == name)
			return nil
		})
		return found
	}

	// While dana is reachable, hen's server asks it where it is (every half hour) and learns another address.
	second, closeSecond := forward(t, dana.server)
	mu.Lock()
	danaSays = []string{"https://" + second}
	mu.Unlock()
	askPeerAddresses(context.Background(), henConfig, nil)
	if got := bookFor(henConfig, info).Addresses(); len(got) < 2 || got[0] != first || !strings.Contains(strings.Join(got, " "), second) {
		t.Fatalf("after asking dana: %v", got)
	}

	// dana's line changes: neither address answers, and dana publishes the new one in the DHT.
	closeFirst()
	closeSecond()
	third, closeThird := forward(t, dana.server)
	defer closeThird()
	var lookedUp int
	lookupPeer = func(ctx context.Context, key ed25519.PublicKey, fp string) (discovery.Record, error) {
		lookedUp++
		if hex.EncodeToString(key) != keyHex || fp != fingerprint {
			return discovery.Record{}, errors.New("looked up the wrong server")
		}
		return discovery.Record{V: 1, Addresses: []string{"https://" + third}}, nil
	}
	defer func() {
		lookupPeer = func(ctx context.Context, key ed25519.PublicKey, fp string) (discovery.Record, error) {
			return dhtNode.Lookup(ctx, key, fp)
		}
	}()

	hen.uploadPhoto(t, client, []byte("a photo"), "IMG_1.jpg")
	runCopy(context.Background(), hen.copiers[0])
	if !copied("IMG_1.jpg") || lookedUp != 1 {
		t.Fatalf("copied: %v, looked up %d times", copied("IMG_1.jpg"), lookedUp)
	}
	info, _, _ = loadPeer(henConfig, "dana")
	if info.Addresses[0] != third {
		t.Errorf("the address that worked isn't kept first: %v", info.Addresses)
	}
}

// danaInvite is a peer invite code from env's admin API, as peer invite gets it.
func danaInvite(t *testing.T, env *testEnv) string {
	t.Helper()
	admin := httptest.NewServer(newAdminMux(env.config, env.enrollment, "secret", env.refreshIndexes, nil, nil, env.peers))
	t.Cleanup(admin.Close)
	tokenFile := filepath.Join(t.TempDir(), "admin.token")
	os.WriteFile(tokenFile, []byte("secret\n"), 0o600)
	config := env.config
	config.AdminAddr, config.AdminTokenFile = strings.TrimPrefix(admin.URL, "http://"), tokenFile
	var invite struct {
		CreatePeerInvite struct{ Code string } `json:"createPeerInvite"`
	}
	if err := adminQuery(config, `mutation { createPeerInvite(name: "hen-home", quota: 1e9) { code } }`, nil, &invite); err != nil {
		t.Fatal(err)
	}
	return invite.CreatePeerInvite.Code
}
