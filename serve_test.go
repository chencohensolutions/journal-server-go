package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/journal/server-go/aesthetic"
	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/copies"
	"github.com/journal/server-go/enroll"
	"github.com/journal/server-go/faces"
	"github.com/journal/server-go/media"
	"github.com/journal/server-go/ocr"
	"github.com/journal/server-go/peers"
	"github.com/journal/server-go/reading"
	"github.com/journal/server-go/search"
	"github.com/journal/server-go/similar"
)

type testEnv struct {
	server     *httptest.Server
	dataFolder string
	enrollment *enroll.Service
	roots      *x509.CertPool
	config     auth.AppConfig
	search     *search.Service
	faces      *faces.Service
	reader     *reading.Reader
	library    *media.Library
	// Reads what journal-server index added ("index": "daily"); nil otherwise.
	refreshIndexes func()
	// The copies in config.json, which the public API shows; nothing copies until a test starts them (startCopies).
	copiers []*copies.Copier
	// Ends when the test does: passes and compares the API starts run until then.
	copiesCtx context.Context
	// The host of other servers' copies, with peers_folder; nil otherwise.
	peers *peers.Host
	// All the copies, config.json's and the swaps' added from the phone.
	copySet *copySet
	swaps   *swapService
}

// fakeFaceModel as config.FaceModel gives a test env face search without the models: the test writes the faces
// into the data folder's .cache/faces/fake.faces itself, before the env starts, and Run is never called.
const fakeFaceModel = "fake"

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// newTestEnv starts a server with one user, "hen", with an empty data folder; configure changes the config
// before the server starts.
func newTestEnv(t *testing.T, configure ...func(*auth.AppConfig)) *testEnv {
	t.Helper()
	now := time.Now()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)

	serverKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serverDER, _ := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, caCert, &serverKey.PublicKey, caKey)

	dir := t.TempDir()

	config := auth.AppConfig{
		PublicURL: "https://journal.test:8080",
		Users: []auth.UserConfig{{
			Username: "hen", DataFolder: filepath.Join(dir, "journal"),
		}},
	}.WithDefaults()
	for _, change := range configure {
		change(&config)
	}
	registry, err := enroll.OpenRegistry(filepath.Join(dir, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	enrollment := enroll.NewService(enroll.NewIssuer(caCert, caKey, time.Now), registry, time.Now)

	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	library, err := openLibrary(config)
	if err != nil {
		t.Fatal(err)
	}
	previews := newPreviews(config)
	readCamerasWith(library, previews)
	var ix indexes
	var searcher *search.Service
	var facer *faces.Service
	var reader *reading.Reader
	var refreshIndexes func()
	if config.Index == auth.IndexDaily {
		// As the server does with "index": "daily": only what queries need, read from caches another process writes.
		ix = openQueryIndexes(config, library)
		refreshIndexes = ix.refresh
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		ix.runs = newDailyRuns(ctx, config, ix.refresh)
	} else {
		// Text search is off unless a test names a model folder: loading one takes seconds. The test runs the reader.
		if config.SearchModel != auth.DefaultSearchModel {
			if searcher = newSearch(config, library); searcher == nil {
				t.Fatalf("no text search with %s", config.SearchModel)
			}
			t.Cleanup(searcher.Close)
		}
		// Face search likewise, with the models a test names, or none (fakeFaceModel).
		switch config.FaceModel {
		case auth.DefaultFaceModel:
		case fakeFaceModel:
			if facer, err = faces.NewService(&faces.Model{Name: fakeFaceModel}, library); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(facer.Close)
		default:
			if facer = newFaces(config, library); facer == nil {
				t.Fatalf("no face search with %s", config.FaceModel)
			}
			t.Cleanup(facer.Close)
		}
		ix = indexes{search: searcher, faces: facer}
		ix.ocr, ix.similar, ix.aesthetic = testIndexes(t, config, library)
		// The reader of the indexes on, which a test runs itself (never with fake models: they can't read).
		if config.FaceModel != fakeFaceModel && config.SimilarModel != fakeModel {
			reader = newReader(config, library, previews, ix)
		}
		ix.reader = reader
		if reader != nil {
			ix.runs = readerRuns{reader}
		}
	}
	copiers, err := newCopiers(config, library)
	if err != nil {
		t.Fatal(err)
	}
	host, err := openPeerHost(config, enrollment.Issuer)
	if err != nil {
		t.Fatal(err)
	}
	copiesCtx, cancelCopies := context.WithCancel(context.Background())
	t.Cleanup(cancelCopies)
	set := newCopySet(copiesCtx, copiers)
	self := &testSelf{}
	swaps := newSwapService(config, library, host, set, self.get)
	server := httptest.NewUnstartedServer(newMux(config, enrollment, library, previews, ix, copyRunner{copiesCtx, set}, host, swaps))
	server.TLS = tlsConfigFor(tls.Certificate{Certificate: [][]byte{serverDER}, PrivateKey: serverKey}, pool)
	server.Config.HTTP2 = &http.HTTP2Config{MaxReceiveBufferPerStream: h2ReceiveWindow, MaxReceiveBufferPerConnection: h2ReceiveWindow}
	server.StartTLS()
	t.Cleanup(server.Close)
	self.fingerprint = peers.Fingerprint(server.Certificate())
	self.found = peers.Discovery{Addresses: []string{server.URL}}
	return &testEnv{server: server, dataFolder: config.Users[0].DataFolder, enrollment: enrollment, roots: pool,
		config: config, search: searcher, faces: facer, reader: reader, refreshIndexes: refreshIndexes, library: library,
		copiers: copiers, copiesCtx: copiesCtx, peers: host, copySet: set, swaps: swaps}
}

// testSelf is a test server's fingerprint and address, for the swap links it makes.
type testSelf struct {
	fingerprint string
	found       peers.Discovery
}

func (s *testSelf) get() (string, peers.Discovery) { return s.fingerprint, s.found }

func (e *testEnv) client(cert *tls.Certificate) *http.Client {
	tlsConfig := &tls.Config{RootCAs: e.roots}
	if cert != nil {
		tlsConfig.Certificates = []tls.Certificate{*cert}
	}
	// Over HTTP/2, as OkHttp and URLSession connect; a custom TLS config turns it off unless asked for.
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true}}
}

type gqlResponse struct {
	Status int
	Data   map[string]json.RawMessage
	Errors []struct {
		Message    string
		Extensions map[string]any
	}
}

func (e *testEnv) graphql(t *testing.T, client *http.Client, query string, variables map[string]any) gqlResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"query": query, "variables": variables})
	resp, err := client.Post(e.server.URL+"/query", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	result := gqlResponse{Status: resp.StatusCode}
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

const enrollMutation = `mutation($input: EnrollDeviceInput!) {
	enrollDevice(input: $input) {
		__typename
		... on EnrolledDevice { user deviceName certificate serial expiresAt }
		... on EnrollmentRejected { reason }
	}
}`

type enrollResult struct {
	Typename    string `json:"__typename"`
	User        string
	DeviceName  string
	Certificate string
	Serial      string
	Reason      string
}

func (e *testEnv) enroll(t *testing.T, code string, key *ecdsa.PrivateKey) enrollResult {
	t.Helper()
	csr, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "device"}}, key)
	resp := e.graphql(t, e.client(nil), enrollMutation, map[string]any{"input": map[string]any{
		"code":          code,
		"deviceName":    "Pixel 9 Pro",
		"csr":           base64.StdEncoding.EncodeToString(csr),
		"keyProtection": "STRONGBOX",
	}})
	if len(resp.Errors) > 0 {
		t.Fatalf("enrollDevice errors: %+v", resp.Errors)
	}
	var result enrollResult
	json.Unmarshal(resp.Data["enrollDevice"], &result)
	return result
}

func TestAnonymousClientsReachOnlyPublicFields(t *testing.T) {
	env := newTestEnv(t)
	anonymous := env.client(nil)

	for _, query := range []string{
		`{ storageUsage { photos } }`,
		`{ __schema { queryType { name } } }`,
		`{ __type(name: "Image") { name } }`,
	} {
		resp := env.graphql(t, anonymous, query, nil)
		if len(resp.Errors) == 0 || !strings.Contains(resp.Errors[0].Message, "client certificate required") {
			t.Errorf("%s: errors = %+v", query, resp.Errors)
		}
	}

	resp := env.graphql(t, anonymous, `{ enrollmentPreview(code: "AAAA-AAAA") { ... on EnrollmentRejected { reason } } }`, nil)
	if len(resp.Errors) > 0 || !strings.Contains(string(resp.Data["enrollmentPreview"]), "INVALID_CODE") {
		t.Fatalf("preview: %+v %s", resp.Errors, resp.Data["enrollmentPreview"])
	}

	mediaResp, err := anonymous.Get(env.server.URL + "/media/" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	mediaResp.Body.Close()
	if mediaResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous media download: %d", mediaResp.StatusCode)
	}
}

func TestEnrolledDeviceCanUseItsCertificateUntilRevoked(t *testing.T) {
	env := newTestEnv(t)
	code, _ := env.enrollment.Codes.Create("hen")

	preview := env.graphql(t, env.client(nil), `query($code: String!) {
		enrollmentPreview(code: $code) { ... on EnrollmentPreview { user expiresAt } }
	}`, map[string]any{"code": strings.ToLower(code)})
	if !strings.Contains(string(preview.Data["enrollmentPreview"]), `"user":"hen"`) {
		t.Fatalf("preview = %s %+v", preview.Data["enrollmentPreview"], preview.Errors)
	}

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	result := env.enroll(t, code, key)
	if result.Typename != "EnrolledDevice" || result.User != "hen" || result.DeviceName != "Pixel 9 Pro" {
		t.Fatalf("enroll = %+v", result)
	}
	if again := env.enroll(t, code, key); again.Reason != "INVALID_CODE" {
		t.Fatalf("second use of the code = %+v", again)
	}

	certDER, _ := base64.StdEncoding.DecodeString(result.Certificate)
	device := env.client(&tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: key})

	usage := env.graphql(t, device, `{ storageUsage { photos videos bytes } }`, nil)
	if len(usage.Errors) > 0 || !strings.Contains(string(usage.Data["storageUsage"]), `"photos":0`) {
		t.Fatalf("storageUsage = %s %+v", usage.Data["storageUsage"], usage.Errors)
	}

	if _, err := env.enrollment.Registry.Revoke(result.Serial, time.Now()); err != nil {
		t.Fatal(err)
	}
	// New connection, so the revoked certificate is checked again.
	revoked := env.client(&tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: key})
	if resp := env.graphql(t, revoked, `{ storageUsage { photos } }`, nil); resp.Status != http.StatusUnauthorized {
		t.Fatalf("revoked certificate: status %d", resp.Status)
	}
}

func TestAdminAPIRequiresTokenAndCreatesCodes(t *testing.T) {
	env := newTestEnv(t)
	admin := httptest.NewServer(newAdminMux(env.config, env.enrollment, "secret", env.refreshIndexes, nil, nil, env.peers))
	defer admin.Close()

	unauthorized, err := http.Post(admin.URL+"/admin/query", "application/json", strings.NewReader(`{"query":"{ devices { serial } }"}`))
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", unauthorized.StatusCode)
	}

	tokenFile := filepath.Join(t.TempDir(), "admin.token")
	os.WriteFile(tokenFile, []byte("secret\n"), 0o600)
	config := env.config
	config.AdminAddr = strings.TrimPrefix(admin.URL, "http://")
	config.AdminTokenFile = tokenFile

	var data struct {
		CreateEnrollmentCode struct{ Code, User string }
	}
	if err := adminQuery(config, `mutation { createEnrollmentCode(user: "hen") { code user } }`, nil, &data); err != nil {
		t.Fatal(err)
	}
	if _, ok := env.enrollment.Codes.Peek(data.CreateEnrollmentCode.Code); !ok || data.CreateEnrollmentCode.User != "hen" {
		t.Fatalf("created code = %+v", data.CreateEnrollmentCode)
	}
	err = adminQuery(config, `mutation { createEnrollmentCode(user: "nobody") { code } }`, nil, &data)
	if err == nil || !strings.Contains(err.Error(), "unknown user") {
		t.Fatalf("unknown user: %v", err)
	}
}

func TestAdminListenerMustBeLoopback(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:8081": true,
		"localhost:8081": true,
		"[::1]:8081":     true,
		"0.0.0.0:8081":   false,
		":8081":          false,
		"192.168.1.2:81": false,
	} {
		if err := requireLoopback(addr); (err == nil) != ok {
			t.Errorf("requireLoopback(%q) = %v", addr, err)
		}
	}
}

func TestDevicesSpeakHTTP2AndHTTP1(t *testing.T) {
	env := newTestEnv(t)
	h2 := env.enrolledClient(t)
	// Its own TLS config: the HTTP/2 transport writes "h2" into the one it is given.
	h1Config := h2.Transport.(*http.Transport).TLSClientConfig.Clone()
	h1Config.NextProtos = nil
	h1 := &http.Client{Transport: &http.Transport{TLSClientConfig: h1Config}}
	for _, c := range []struct {
		client *http.Client
		proto  string
	}{{h2, "HTTP/2.0"}, {h1, "HTTP/1.1"}} {
		body, _ := json.Marshal(map[string]any{"query": "{ storageUsage { bytes } }"})
		resp, err := c.client.Post(env.server.URL+"/query", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Proto != c.proto {
			t.Fatalf("storageUsage over %s: %d %s", c.proto, resp.StatusCode, resp.Proto)
		}
	}
}

// fakeModel as config.SimilarModel, OCRModel or AestheticModel gives a test env that index without its model: the
// test writes what it would have read into the data folder's .cache itself, before the env starts.
const fakeModel = "fake"

// testIndexes is the OCR, similar and aesthetic indexes a test asked for: with fakeModel, without models; off
// otherwise (loading them takes seconds), unless a test names a real model folder.
func testIndexes(t *testing.T, config auth.AppConfig, library *media.Library) (*ocr.Service, *similar.Service, *aesthetic.Service) {
	t.Helper()
	var (
		o   *ocr.Service
		s   *similar.Service
		a   *aesthetic.Service
		err error
	)
	switch config.OCRModel {
	case auth.DefaultOCRModel:
	case fakeModel:
		if o, err = ocr.NewService(&ocr.Model{Name: fakeModel}, library); err != nil {
			t.Fatal(err)
		}
	default:
		if o = newOCR(config, library); o == nil {
			t.Fatalf("no OCR with %s", config.OCRModel)
		}
	}
	switch config.SimilarModel {
	case auth.DefaultSimilarModel:
	case fakeModel:
		if s, err = similar.NewService(&similar.Model{Name: fakeModel, Dim: 2}, library); err != nil {
			t.Fatal(err)
		}
	default:
		if s = newSimilar(config, library); s == nil {
			t.Fatalf("no similar photos with %s", config.SimilarModel)
		}
	}
	switch config.AestheticModel {
	case auth.DefaultAestheticModel:
	case fakeModel:
		if a, err = aesthetic.NewService(&aesthetic.Model{Name: fakeModel}, library); err != nil {
			t.Fatal(err)
		}
	default:
		if a = newAesthetic(config, library); a == nil {
			t.Fatalf("no aesthetic scores with %s", config.AestheticModel)
		}
	}
	if o != nil {
		t.Cleanup(o.Close)
	}
	if s != nil {
		t.Cleanup(s.Close)
	}
	if a != nil {
		t.Cleanup(a.Close)
	}
	return o, s, a
}
