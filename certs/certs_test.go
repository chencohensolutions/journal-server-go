package certs

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestMain(m *testing.M) {
	caKeyBits = 2048
	os.Exit(m.Run())
}

func readCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestCAAndServerCert(t *testing.T) {
	dir := t.TempDir()
	caCert, caKey := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem")
	ca, err := NewCA(caCert, caKey)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.Cert.IsCA || ca.Cert.KeyUsage&x509.KeyUsageCertSign == 0 || len(ca.Cert.SubjectKeyId) == 0 {
		t.Fatalf("not a CA iOS accepts: IsCA %v, usage %v", ca.Cert.IsCA, ca.Cert.KeyUsage)
	}
	if info, _ := os.Stat(caKey); info.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("CA key is readable by others: %v", info.Mode())
	}

	// A new CA never replaces one that is there.
	if _, err := NewCA(caCert, filepath.Join(dir, "other-key.pem")); err == nil {
		t.Fatal("NewCA replaced an existing CA certificate")
	}

	loaded, err := LoadCA(caCert, caKey)
	if err != nil {
		t.Fatal(err)
	}
	serverCert, serverKey := filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem")
	names := []string{"localhost", "family-pc", "192.168.50.7"}
	if err := NewServerCert(loaded, serverCert, serverKey, names); err != nil {
		t.Fatal(err)
	}
	server := readCert(t, serverCert)
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	for _, name := range names {
		if _, err := server.Verify(x509.VerifyOptions{DNSName: name, Roots: roots}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !slices.Contains(server.ExtKeyUsage, x509.ExtKeyUsageServerAuth) {
		t.Error("server certificate lacks serverAuth")
	}
	if server.NotAfter.Sub(server.NotBefore) > ServerValidity+2*3600e9 {
		t.Errorf("server certificate valid for %v, longer than iOS allows", server.NotAfter.Sub(server.NotBefore))
	}

	missing, err := Missing(serverCert, []string{"192.168.50.7", "192.168.50.8", "family-pc", "other"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(missing, []string{"192.168.50.8", "other"}) {
		t.Errorf("Missing = %v", missing)
	}
}

func TestLoadCARefusesWrongKeyAndNonCA(t *testing.T) {
	dir := t.TempDir()
	a, err := NewCA(filepath.Join(dir, "a.pem"), filepath.Join(dir, "a-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewCA(filepath.Join(dir, "b.pem"), filepath.Join(dir, "b-key.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCA(filepath.Join(dir, "a.pem"), filepath.Join(dir, "b-key.pem")); err == nil {
		t.Error("LoadCA took another CA's key")
	}

	// A server certificate is not a CA, even with its own key.
	if err := NewServerCert(a, filepath.Join(dir, "s.pem"), filepath.Join(dir, "s-key.pem"), []string{"localhost"}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCA(filepath.Join(dir, "s.pem"), filepath.Join(dir, "s-key.pem")); err == nil {
		t.Error("LoadCA took a server certificate as a CA")
	}
}
