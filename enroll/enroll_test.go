package enroll

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newClock() *clock { return &clock{t: time.Date(2026, 10, 17, 14, 22, 0, 0, time.UTC)} }

func testCA(t *testing.T, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return cert, key
}

func csrFor(t *testing.T, key any, commonName string) []byte {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: commonName},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func newTestService(t *testing.T, c *clock) *Service {
	t.Helper()
	caCert, caKey := testCA(t, c.t)
	registry, err := OpenRegistry(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	return NewService(NewIssuer(caCert, caKey, c.now), registry, c.now)
}

func TestCodeFormatAndNormalization(t *testing.T) {
	c := newClock()
	store := NewCodeStore(c.now)
	code, expiresAt := store.Create("hen")

	if len(code) != 9 || code[4] != '-' || strings.Trim(strings.Replace(code, "-", "", 1), codeAlphabet) != "" {
		t.Fatalf("unexpected code format %q", code)
	}
	if !expiresAt.Equal(c.t.Add(CodeTTL)) {
		t.Fatalf("expiresAt = %v", expiresAt)
	}

	typed := strings.ToLower(strings.Replace(code, "-", " ", 1))
	typed = strings.NewReplacer("0", "o", "1", "l").Replace(typed)
	if pending, ok := store.Peek(typed); !ok || pending.User != "hen" {
		t.Fatalf("Peek(%q) = %v, %v", typed, pending, ok)
	}
}

func TestCodeIsSingleUse(t *testing.T) {
	store := NewCodeStore(newClock().now)
	code, _ := store.Create("hen")
	if _, ok := store.Peek(code); !ok {
		t.Fatal("Peek should not use up the code")
	}
	if _, ok := store.Redeem(code); !ok {
		t.Fatal("first Redeem failed")
	}
	if _, ok := store.Redeem(code); ok {
		t.Fatal("second Redeem succeeded")
	}
}

func TestCodeExpires(t *testing.T) {
	c := newClock()
	store := NewCodeStore(c.now)
	code, _ := store.Create("hen")
	c.t = c.t.Add(CodeTTL)
	if _, ok := store.Redeem(code); ok {
		t.Fatal("expired code was accepted")
	}
}

func TestMalformedCodesAreRejected(t *testing.T) {
	store := NewCodeStore(newClock().now)
	for _, code := range []string{"", "K7QM-4XP", "K7QM-4XPDD", "K7QM-4XP!", "K7QM-4XPU"} {
		if _, ok := store.Peek(code); ok {
			t.Errorf("Peek(%q) succeeded", code)
		}
	}
}

func TestEnrollIssuesClientCertificateForCodeUser(t *testing.T) {
	c := newClock()
	svc := newTestService(t, c)
	code, _ := svc.Codes.Create("hen")
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	enrolled, rejection, err := svc.Enroll("10.0.0.5", Request{
		Code:          code,
		DeviceName:    "  Pixel 9 Pro\n",
		CSR:           csrFor(t, key, "someone-else"),
		KeyProtection: "STRONGBOX",
	})
	if err != nil || rejection != "" {
		t.Fatalf("Enroll = %v, %v", rejection, err)
	}

	cert, err := x509.ParseCertificate(enrolled.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != "hen" {
		t.Errorf("CN = %q, want the code's user, not the CSR subject", cert.Subject.CommonName)
	}
	if !cert.PublicKey.(*ecdsa.PublicKey).Equal(&key.PublicKey) {
		t.Error("certificate is not for the requested key")
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("ExtKeyUsage = %v", cert.ExtKeyUsage)
	}
	if want := c.t.Add(CertificateValidity); !cert.NotAfter.Equal(want) {
		t.Errorf("NotAfter = %v, want %v", cert.NotAfter, want)
	}

	devices := svc.Registry.List("hen")
	if len(devices) != 1 || devices[0].Name != "Pixel 9 Pro" || devices[0].Serial != SerialString(cert.SerialNumber) || devices[0].KeyProtection != "STRONGBOX" {
		t.Fatalf("registry = %+v", devices)
	}

	if _, rejection, _ := svc.Enroll("10.0.0.5", Request{Code: code, CSR: csrFor(t, key, "x")}); rejection != RejectInvalidCode {
		t.Fatalf("reusing the code: rejection = %q", rejection)
	}
}

func TestInvalidCSRDoesNotUseUpCode(t *testing.T) {
	svc := newTestService(t, newClock())
	code, _ := svc.Codes.Create("hen")
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	p256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tampered := csrFor(t, p256, "x")
	tampered[len(tampered)-1] ^= 1

	for name, csr := range map[string][]byte{
		"garbage":  []byte("not a csr"),
		"rsa":      csrFor(t, rsaKey, "x"),
		"p384":     csrFor(t, p384, "x"),
		"tampered": tampered,
	} {
		if _, rejection, _ := svc.Enroll("ip", Request{Code: code, CSR: csr}); rejection != RejectInvalidCSR {
			t.Errorf("%s: rejection = %q", name, rejection)
		}
	}
	if _, ok := svc.Codes.Peek(code); !ok {
		t.Fatal("code was used up by an invalid request")
	}
}

func TestWrongCodesAreRateLimitedPerIP(t *testing.T) {
	c := newClock()
	svc := newTestService(t, c)
	code, _ := svc.Codes.Create("hen")

	for i := 0; i < maxFailures; i++ {
		if _, rejection := svc.Preview("1.2.3.4", "AAAA-AAAA"); rejection != RejectInvalidCode {
			t.Fatalf("attempt %d: %q", i, rejection)
		}
	}
	if _, rejection := svc.Preview("1.2.3.4", code); rejection != RejectRateLimited {
		t.Fatalf("after %d failures: %q", maxFailures, rejection)
	}
	if _, rejection := svc.Preview("5.6.7.8", code); rejection != "" {
		t.Fatalf("other IP: %q", rejection)
	}

	c.t = c.t.Add(failureWindow + time.Second)
	if preview, rejection := svc.Preview("1.2.3.4", code); rejection != "" || preview.User != "hen" {
		t.Fatalf("after the window: %v, %q", preview, rejection)
	}
}

func TestRegistryPersistsAndRevokes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	registry, _ := OpenRegistry(path)
	now := newClock().t
	if err := registry.Add(Device{Serial: "abc", User: "hen", Name: "Pixel", RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	if registry.IsRevoked("abc") {
		t.Fatal("new device is revoked")
	}
	if _, err := registry.Revoke("abc", now); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Revoke("missing", now); err != ErrDeviceNotFound {
		t.Fatalf("Revoke(missing) = %v", err)
	}

	reopened, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.IsRevoked("abc") || len(reopened.List("")) != 1 {
		t.Fatalf("reopened registry = %+v", reopened.List(""))
	}
	if reopened.IsRevoked("unknown") {
		t.Fatal("certificates outside the registry must not count as revoked")
	}
}

func TestCleanDeviceName(t *testing.T) {
	if got := cleanDeviceName(" \t "); got != "Unnamed device" {
		t.Errorf("blank = %q", got)
	}
	long := strings.Repeat("é", 100)
	if got := cleanDeviceName(long); got != strings.Repeat("é", maxDeviceNameLength) {
		t.Errorf("long name = %q", got)
	}
}
