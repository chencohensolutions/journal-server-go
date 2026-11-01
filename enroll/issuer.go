package enroll

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"time"
)

// CertificateValidity is the lifetime of an issued device certificate (capped by the CA's own expiry).
const CertificateValidity = 365 * 24 * time.Hour

// Issuer signs device certificates with the client CA.
type Issuer struct {
	caCert *x509.Certificate
	caKey  crypto.Signer
	now    func() time.Time
}

func LoadIssuer(certPath, keyPath string, now func() time.Time) (*Issuer, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read CA certificate: %w", err)
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("%s: no PEM certificate", certPath)
	}
	caCert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read CA key: %w", err)
	}
	caKey, err := parsePrivateKey(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", keyPath, err)
	}
	if !caKey.Public().(interface{ Equal(crypto.PublicKey) bool }).Equal(caCert.PublicKey) {
		return nil, fmt.Errorf("%s does not belong to %s", keyPath, certPath)
	}
	return &Issuer{caCert: caCert, caKey: caKey, now: now}, nil
}

// CA is the issuer's CA certificate and its private key.
func (i *Issuer) CA() (*x509.Certificate, crypto.Signer) {
	return i.caCert, i.caKey
}

func NewIssuer(caCert *x509.Certificate, caKey crypto.Signer, now func() time.Time) *Issuer {
	return &Issuer{caCert: caCert, caKey: caKey, now: now}
}

func parsePrivateKey(keyPEM []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("no PEM private key")
	}
	var key any
	var err error
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("unsupported PEM block %q", block.Type)
	}
	if err != nil {
		return nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("unsupported key type %T", key)
	}
	return signer, nil
}

// ParseDeviceCSR accepts only a correctly self-signed request for an EC P-256 key,
// the key type both Android Keystore and the Secure Enclave create.
func ParseDeviceCSR(der []byte) (*x509.CertificateRequest, error) {
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, err
	}
	key, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("certificate request must hold an EC P-256 key")
	}
	return csr, nil
}

// Issue signs a client certificate for csr's key. The subject comes from user, never from the
// request, because the enrollment code alone decides who the device belongs to.
func (i *Issuer) Issue(csr *x509.CertificateRequest, user string) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := i.now()
	notAfter := now.Add(CertificateValidity)
	if notAfter.After(i.caCert.NotAfter) {
		notAfter = i.caCert.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: user},
		NotBefore:    now.Add(-5 * time.Minute), // tolerate clock skew on the device
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, i.caCert, csr.PublicKey, i.caKey)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}
