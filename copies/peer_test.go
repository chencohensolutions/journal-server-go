package copies

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/enroll"
	"github.com/journal/server-go/peers"
)

// testHost is a friend's server keeping copies: the peer API on a TLS server of its own.
type testHost struct {
	host        *peers.Host
	server      *httptest.Server
	fingerprint string
}

func newTestHost(t *testing.T) *testHost {
	t.Helper()
	now := time.Now()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "host-ca"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	serverKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serverDER, _ := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(2),
		Subject: pkix.Name{CommonName: "127.0.0.1"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}, caCert, &serverKey.PublicKey, caKey)
	serverCert, _ := x509.ParseCertificate(serverDER)

	dir := t.TempDir()
	registry, err := peers.OpenRegistry(filepath.Join(dir, "peers.json"))
	if err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(dir, "peers")
	os.Mkdir(folder, 0o755)
	host := peers.NewHost(folder, registry, enroll.NewIssuer(caCert, caKey, time.Now), time.Now)
	mux := http.NewServeMux()
	host.Mount(mux)
	server := httptest.NewUnstartedServer(mux)
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{serverDER}, PrivateKey: serverKey}},
		ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: pool, NextProtos: []string{"h2", "http/1.1"}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return &testHost{host: host, server: server, fingerprint: peers.Fingerprint(serverCert)}
}

// enroll invites a peer named name and enrolls it as peer add does; its certificate.
func (h *testHost) enroll(t *testing.T, name string, quota int64) tls.Certificate {
	t.Helper()
	code, _, err := h.host.Invite(name, quota)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csr, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	body, _ := json.Marshal(peers.EnrollRequest{Code: code, CSR: csr})
	resp, err := PeerClient(tls.Certificate{}, h.fingerprint).Post(h.server.URL+"/peer/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var enrolled peers.Enrolled
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&enrolled) != nil || enrolled.Name != name {
		t.Fatalf("enroll: %s %+v", resp.Status, enrolled)
	}
	return tls.Certificate{Certificate: [][]byte{enrolled.Certificate}, PrivateKey: key}
}

func (h *testHost) peer(t *testing.T, cert tls.Certificate, user string) *Peer {
	return NewPeer("dana", h.server.URL, user, cert, h.fingerprint)
}

func TestPeerTarget(t *testing.T) {
	h := newTestHost(t)
	testTarget(t, h.peer(t, h.enroll(t, "hen-home", 1<<30), "hen"))
}

func TestPeerRefusesAnotherServerKey(t *testing.T) {
	h := newTestHost(t)
	cert := h.enroll(t, "hen-home", 1<<30)
	p := NewPeer("dana", h.server.URL, "hen", cert, strings.Repeat("0", 64))
	if err := p.Ready(); !errors.Is(err, ErrUnreachable) || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("a server with another key: %v", err)
	}
}

func TestAPassToAPeer(t *testing.T) {
	h := newTestHost(t)
	store, _ := newCopier(t)
	addPhoto(t, store, "IMG_1.jpg", "a photo for the peer")
	addPhoto(t, store, "IMG_2.jpg", "another photo")
	p := h.peer(t, h.enroll(t, "hen-home", 1<<30), "hen")
	c := NewTo("hen", store, auth.CopyConfig{Peer: "dana", At: "03:30"}, p, time.Now)
	if c.Folder != "dana" || c.PeerName() != "dana" {
		t.Fatalf("a peer's copy is named %q, %q", c.Folder, c.PeerName())
	}
	if same, err := c.SameDisk(); same || err != nil {
		t.Fatalf("a peer on the data folder's disk: %v %v", same, err)
	}
	pass := c.Run(context.Background())
	if pass.Err != nil || pass.Failed != 0 || pass.Files < 4 {
		t.Fatalf("the pass: %+v", pass)
	}
	// The host keeps the data folder's layout in its space: <peers_folder>/<peer>/<user>/.
	space := filepath.Join(h.host.Folder, "hen-home", "hen")
	want := tree(t, filepath.Join(store.Root(), "media"))
	got := tree(t, filepath.Join(space, "media"))
	for path, file := range want {
		if got[path] != file {
			t.Errorf("%s at the peer: %q, want %q", path, got[path], file)
		}
	}
	if _, err := os.Stat(filepath.Join(space, savedName)); err != nil {
		t.Errorf("the pass wasn't saved at the peer: %v", err)
	}
	// A new copier (a restart) asks the host, not its own memory.
	again := NewTo("hen", store, auth.CopyConfig{Peer: "dana", At: "03:30"}, h.peer(t, p.certificate(t), "hen"), time.Now)
	if _, last := again.Status(); last == nil || last.Files != pass.Files {
		t.Errorf("the last pass after a restart: %+v", last)
	}
	report, err := again.Check(context.Background(), true)
	if err != nil || report.Originals != 0 || report.Sidecars != 0 || report.Total != 2 {
		t.Fatalf("after the pass: %+v %v", report, err)
	}
	if pass := again.Run(context.Background()); pass.Files != 0 || pass.Err != nil {
		t.Errorf("a second pass copied %+v", pass)
	}
	if space := h.host.SpaceOf(mustPeer(t, h, "hen-home")); space.Used == 0 || space.Free >= space.Quota {
		t.Errorf("the host's count: %+v", space)
	}
}

func (p *Peer) certificate(t *testing.T) tls.Certificate {
	return p.client.Transport.(*http.Transport).TLSClientConfig.Certificates[0]
}

func mustPeer(t *testing.T, h *testHost, name string) peers.Peer {
	t.Helper()
	p, ok := h.host.Registry.Active(name)
	if !ok {
		t.Fatalf("no peer %s", name)
	}
	return p
}

func TestAPassStopsWhenThePeerQuotaIsFull(t *testing.T) {
	h := newTestHost(t)
	store, _ := newCopier(t)
	addPhoto(t, store, "IMG_1.jpg", strings.Repeat("x", 100))
	c := NewTo("hen", store, auth.CopyConfig{Peer: "dana"}, h.peer(t, h.enroll(t, "hen-home", 50), "hen"), time.Now)
	pass := c.Run(context.Background())
	if !errors.Is(pass.Err, ErrQuotaFull) {
		t.Fatalf("a pass past the quota: %+v", pass)
	}
	if _, last := c.Status(); !errors.Is(last.Err, ErrQuotaFull) {
		t.Errorf("the pass's error: %v", last.Err)
	}
}

func TestAPassStopsWhenThePeerIsRevoked(t *testing.T) {
	h := newTestHost(t)
	store, _ := newCopier(t)
	addPhoto(t, store, "IMG_1.jpg", "a photo")
	c := NewTo("hen", store, auth.CopyConfig{Peer: "dana"}, h.peer(t, h.enroll(t, "hen-home", 1<<30), "hen"), time.Now)
	if _, err := h.host.Registry.Revoke("hen-home", time.Now()); err != nil {
		t.Fatal(err)
	}
	pass := c.Run(context.Background())
	if !errors.Is(pass.Err, ErrUnreachable) || c.UnreachableSince().IsZero() {
		t.Fatalf("a pass to a revoked peer: %+v, since %v", pass, c.UnreachableSince())
	}
}

func TestAPutToAPeerResumes(t *testing.T) {
	defer func(was int64) { resumeFrom = was }(resumeFrom)
	resumeFrom = 10
	h := newTestHost(t)
	p := h.peer(t, h.enroll(t, "hen-home", 1<<30), "hen")
	data := []byte("the bytes of a video, sent in two tries")
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	// The first try stopped after 12 bytes: they are kept at the host by the file's sha256.
	req, _ := http.NewRequest(http.MethodPut, p.url("files", "media/2026/2026-10/VID.mp4"), bytes.NewReader(data[:12]))
	req.Header.Set(peers.HeaderSha256, sha)
	req.Header.Set(peers.HeaderModified, taken.Format(time.RFC3339Nano))
	req.Header.Set("Content-Range", fmt.Sprintf("bytes 0-11/%d", len(data)))
	resp, err := p.client.Do(req)
	if err != nil || resp.StatusCode != http.StatusAccepted || resp.Header.Get(peers.HeaderOffset) != "12" {
		t.Fatalf("half a file: %v %v", resp, err)
	}
	if offset, err := p.offset(sha); offset != 12 || err != nil {
		t.Fatalf("the host has %d, %v", offset, err)
	}
	// The second sends only the rest, though it reads the file from its start.
	var read int
	counted := readFunc(func(b []byte) (int, error) { n, err := bytes.NewReader(data[read:]).Read(b); read += n; return n, err })
	if err := p.Put("media/2026/2026-10/VID.mp4", counted, int64(len(data)), sha, taken); err != nil {
		t.Fatal(err)
	}
	got, err := readAll(p, "media/2026/2026-10/VID.mp4")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("at the peer: %q, %v", got, err)
	}
	if f, err := p.Stat("media/2026/2026-10/VID.mp4"); err != nil || !f.ModTime.Equal(taken) {
		t.Errorf("its time: %+v %v", f, err)
	}
}

type readFunc func([]byte) (int, error)

func (f readFunc) Read(b []byte) (int, error) { return f(b) }

func TestAPeerCertificateReachesOnlyItsOwnSpace(t *testing.T) {
	h := newTestHost(t)
	hen := h.peer(t, h.enroll(t, "hen-home", 1<<30), "hen")
	sum := sha256.Sum256([]byte("hen's"))
	if err := hen.Put("settings.json", bytes.NewReader([]byte("hen's")), 5, hex.EncodeToString(sum[:]), taken); err != nil {
		t.Fatal(err)
	}
	other := h.peer(t, h.enroll(t, "noa-home", 1<<30), "hen")
	if _, err := readAll(other, "settings.json"); err == nil {
		t.Error("another peer read hen-home's file")
	}
	// No certificate, or a device's, is refused.
	resp, err := PeerClient(tls.Certificate{}, h.fingerprint).Get(h.server.URL + "/peer/space")
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no certificate: %v %v", resp, err)
	}
	// Paths out of the space are refused.
	for _, bad := range []string{"../noa-home/hen/settings.json", ".journal-partial/x", "media/../../x"} {
		if _, err := readAll(hen, bad); err == nil {
			t.Errorf("%s was read", bad)
		}
	}
}
