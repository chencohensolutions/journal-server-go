package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const renewMutation = `mutation($csr: String!) {
	renewCertificate(csr: $csr, keyProtection: TEE) {
		__typename
		... on EnrolledDevice { user deviceName certificate serial expiresAt }
		... on EnrollmentRejected { reason }
	}
}`

// A device renews its certificate with a new key: the same user and device, its settings kept; the old certificate
// works until the new one is used, then never again.
func TestADeviceRenewsItsCertificate(t *testing.T) {
	env := newTestEnv(t)
	code, _ := env.enrollment.Codes.Create("hen")
	oldKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	enrolled := env.enroll(t, code, oldKey)
	oldDER, _ := base64.StdEncoding.DecodeString(enrolled.Certificate)
	oldClient := env.client(&tls.Certificate{Certificate: [][]byte{oldDER}, PrivateKey: oldKey})
	if _, err := env.enrollment.Registry.SetWithholdPrivate(enrolled.Serial, true); err != nil {
		t.Fatal(err)
	}
	if info := env.graphql(t, oldClient, `{ serverInfo { renewCertificates } }`, nil); !strings.Contains(string(info.Data["serverInfo"]), "true") {
		t.Fatalf("serverInfo: %s", info.Data["serverInfo"])
	}

	newKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csr, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "device"}}, newKey)
	resp := env.graphql(t, oldClient, renewMutation, map[string]any{"csr": base64.StdEncoding.EncodeToString(csr)})
	var renewed struct {
		Typename    string `json:"__typename"`
		User        string
		DeviceName  string
		Certificate string
		Serial      string
		ExpiresAt   time.Time
	}
	if len(resp.Errors) > 0 || json.Unmarshal(resp.Data["renewCertificate"], &renewed) != nil || renewed.Typename != "EnrolledDevice" {
		t.Fatalf("renew: %s %+v", resp.Data["renewCertificate"], resp.Errors)
	}
	// (The test CA lasts a day, which caps the certificate.)
	if renewed.User != "hen" || renewed.DeviceName != "Pixel 9 Pro" || renewed.Serial == enrolled.Serial ||
		time.Until(renewed.ExpiresAt) < 20*time.Hour {
		t.Errorf("the renewed certificate: %+v", renewed)
	}
	device, _ := env.enrollment.Registry.Find(renewed.Serial)
	if !device.WithholdPrivate || device.RenewedFrom != enrolled.Serial || device.KeyProtection != "TEE" {
		t.Errorf("the renewed device: %+v", device)
	}

	// The old certificate still works (the device hasn't switched yet)...
	if r := env.graphql(t, oldClient, `{ storageUsage { photos } }`, nil); len(r.Errors) > 0 || r.Status != 200 {
		t.Fatalf("the old certificate before the switch: %+v", r)
	}
	// ...until the new one is used.
	newDER, _ := base64.StdEncoding.DecodeString(renewed.Certificate)
	newClient := env.client(&tls.Certificate{Certificate: [][]byte{newDER}, PrivateKey: newKey})
	if r := env.graphql(t, newClient, `{ storageUsage { photos } }`, nil); len(r.Errors) > 0 || r.Status != 200 {
		t.Fatalf("the new certificate: %+v", r)
	}
	oldClient = env.client(&tls.Certificate{Certificate: [][]byte{oldDER}, PrivateKey: oldKey}) // a new connection
	if r := env.graphql(t, oldClient, `{ storageUsage { photos } }`, nil); r.Status != 401 {
		t.Errorf("the old certificate after the switch: %d %+v", r.Status, r.Errors)
	}
	// A revoked certificate can't renew.
	if r := env.graphql(t, oldClient, renewMutation, map[string]any{"csr": base64.StdEncoding.EncodeToString(csr)}); r.Status != 401 {
		t.Errorf("a revoked certificate renewed: %d %s", r.Status, r.Data["renewCertificate"])
	}
	// One made outside enrollment (not in the registry) can't either.
	outsideKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	outsideCSR, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, outsideKey)
	parsed, _ := x509.ParseCertificateRequest(outsideCSR)
	outside, err := env.enrollment.Issuer.Issue(parsed, "hen")
	if err != nil {
		t.Fatal(err)
	}
	outsideClient := env.client(&tls.Certificate{Certificate: [][]byte{outside.Raw}, PrivateKey: outsideKey})
	resp = env.graphql(t, outsideClient, renewMutation, map[string]any{"csr": base64.StdEncoding.EncodeToString(csr)})
	if !strings.Contains(string(resp.Data["renewCertificate"]), "NOT_ENROLLED") {
		t.Errorf("a certificate from outside enrollment: %s %+v", resp.Data["renewCertificate"], resp.Errors)
	}
}
