package main

import (
	"bytes"
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

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/enroll"
)

type testEnv struct {
	server     *httptest.Server
	dataFolder string
	enrollment *enroll.Service
	roots      *x509.CertPool
	config     auth.AppConfig
}

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

func newTestEnv(t *testing.T) *testEnv {
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
	imageFolder := filepath.Join(dir, "images")
	os.Mkdir(imageFolder, 0o755)
	os.WriteFile(filepath.Join(imageFolder, "beach.jpg"), []byte("jpeg"), 0o644)

	config := auth.AppConfig{
		PublicURL: "https://journal.test:8080",
		Users: []auth.UserConfig{{
			Username: "hen", CertCommonName: "hen", ImageFolder: imageFolder, DataFolder: filepath.Join(dir, "journal"),
		}},
	}.WithDefaults()
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
	server := httptest.NewUnstartedServer(newMux(config, enrollment, library, newPreviews(config)))
	server.TLS = &tls.Config{
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    pool,
		Certificates: []tls.Certificate{{Certificate: [][]byte{serverDER}, PrivateKey: serverKey}},
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return &testEnv{server: server, dataFolder: config.Users[0].DataFolder, enrollment: enrollment, roots: pool, config: config}
}

func (e *testEnv) client(cert *tls.Certificate) *http.Client {
	tlsConfig := &tls.Config{RootCAs: e.roots}
	if cert != nil {
		tlsConfig.Certificates = []tls.Certificate{*cert}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}
}

type gqlResponse struct {
	Status int
	Data   map[string]json.RawMessage
	Errors []struct{ Message string }
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
		`{ images { id } }`,
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

	imageResp, err := anonymous.Get(env.server.URL + "/images/beach.jpg")
	if err != nil {
		t.Fatal(err)
	}
	imageResp.Body.Close()
	if imageResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous image download: %d", imageResp.StatusCode)
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

	images := env.graphql(t, device, `{ images { id url } }`, nil)
	if len(images.Errors) > 0 || !strings.Contains(string(images.Data["images"]), "beach.jpg") {
		t.Fatalf("images = %s %+v", images.Data["images"], images.Errors)
	}
	imageResp, err := device.Get(env.server.URL + "/images/beach.jpg")
	if err != nil {
		t.Fatal(err)
	}
	imageResp.Body.Close()
	if imageResp.StatusCode != http.StatusOK {
		t.Fatalf("image download: %d", imageResp.StatusCode)
	}

	if _, err := env.enrollment.Registry.Revoke(result.Serial, time.Now()); err != nil {
		t.Fatal(err)
	}
	// New connection, so the revoked certificate is checked again.
	revoked := env.client(&tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: key})
	if resp := env.graphql(t, revoked, `{ images { id } }`, nil); resp.Status != http.StatusUnauthorized {
		t.Fatalf("revoked certificate: status %d", resp.Status)
	}
}

func TestAdminAPIRequiresTokenAndCreatesCodes(t *testing.T) {
	env := newTestEnv(t)
	admin := httptest.NewServer(newAdminMux(env.config, env.enrollment, "secret"))
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
