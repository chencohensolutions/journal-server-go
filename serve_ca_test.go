package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// A phone enrolled at a server with its own CA keeps the CA serverInfo gives, and from then on trusts the server by
// it alone: at an address its certificate doesn't list, and without the CA built into the app.
func TestServerInfoGivesTheServersCA(t *testing.T) {
	env := newTestEnv(t)
	code, _ := env.enrollment.Codes.Create("hen")
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	result := env.enroll(t, code, key)
	if result.Typename != "EnrolledDevice" {
		t.Fatalf("enroll = %+v", result)
	}
	certDER, _ := base64.StdEncoding.DecodeString(result.Certificate)
	device := env.client(&tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: key})
	info := env.graphql(t, device, `{ serverInfo { caCertificate } }`, nil)
	var got struct{ CaCertificate string }
	json.Unmarshal(info.Data["serverInfo"], &got)
	ca, _ := env.enrollment.Issuer.CA()
	kept := x509.NewCertPool()
	if !kept.AppendCertsFromPEM([]byte(got.CaCertificate)) || !strings.Contains(got.CaCertificate, base64.StdEncoding.EncodeToString(ca.Raw)[:60]) {
		t.Fatalf("caCertificate = %q %+v", got.CaCertificate, info.Errors)
	}

	// What the app does with the kept CA: the chain must lead to it and be for a server; the host name isn't checked.
	byCA := &tls.Config{
		Certificates:       []tls.Certificate{{Certificate: [][]byte{certDER}, PrivateKey: key}},
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no certificate")
			}
			intermediates := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				intermediates.AddCert(c)
			}
			_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: kept, Intermediates: intermediates,
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
			return err
		},
	}
	byKeptCA := &http.Client{Transport: &http.Transport{TLSClientConfig: byCA}}
	// The test server's certificate lists 127.0.0.1 only.
	env.server.URL = strings.Replace(env.server.URL, "127.0.0.1", "localhost", 1)
	usage := env.graphql(t, byKeptCA, `{ storageUsage { photos } }`, nil)
	if len(usage.Errors) > 0 || usage.Status != http.StatusOK {
		t.Fatalf("by the kept CA: %d %+v", usage.Status, usage.Errors)
	}
}
