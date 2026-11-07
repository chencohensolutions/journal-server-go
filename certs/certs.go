// Package certs makes the server's certificates without OpenSSL, for journal-server setup: the CA that signs the
// server certificate and every enrolled device's certificate (NewCA), and the server's TLS certificate for the names
// and addresses devices reach it by (NewServerCert). They match what scripts/generate-ca.sh and
// generate-server-cert.sh make. See docs/install.md.
package certs

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/journal/server-go/enroll"
)

const (
	// CAValidity is the CA's lifetime, as in generate-ca.sh.
	CAValidity = 10 * 365 * 24 * time.Hour
	// ServerValidity is the server certificate's lifetime: iOS refuses TLS server certificates valid for longer
	// than 825 days.
	ServerValidity = 825 * 24 * time.Hour
)

// caKeyBits is the CA key's size; tests make smaller ones, which are much quicker.
var caKeyBits = 4096

// CA is the certificate authority whose certificate the apps trust and whose key signs certificates.
type CA struct {
	Cert *x509.Certificate
	Key  crypto.Signer
}

// LoadCA reads a CA certificate and its private key (PEM), and checks the key belongs to it and that it is a CA.
func LoadCA(certPath, keyPath string) (CA, error) {
	issuer, err := enroll.LoadIssuer(certPath, keyPath, time.Now)
	if err != nil {
		return CA{}, err
	}
	cert, key := issuer.CA()
	if !cert.IsCA || cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return CA{}, fmt.Errorf("%s is not a CA certificate", certPath)
	}
	if time.Now().After(cert.NotAfter) {
		return CA{}, fmt.Errorf("%s expired on %s", certPath, cert.NotAfter.Format(time.DateOnly))
	}
	return CA{cert, key}, nil
}

// NewCA makes a CA (RSA 4096, CN=journal-server-ca) and writes its certificate and key. It refuses to replace
// either file: a new CA invalidates the server certificate and every enrolled device.
func NewCA(certPath, keyPath string) (CA, error) {
	for _, p := range []string{certPath, keyPath} {
		if _, err := os.Stat(p); err == nil {
			return CA{}, fmt.Errorf("%s already exists; a new CA would invalidate every enrolled device", p)
		}
	}
	key, err := rsa.GenerateKey(rand.Reader, caKeyBits)
	if err != nil {
		return CA{}, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "journal-server-ca"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(CAValidity),
		// Marked as a CA explicitly, with its own key id: iOS refuses a trust anchor without these.
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		SubjectKeyId:          keyID(&key.PublicKey),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return CA{}, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return CA{}, err
	}
	if err := writeKeyAndCert(keyPath, key, certPath, der); err != nil {
		return CA{}, err
	}
	return CA{cert, key}, nil
}

// NewServerCert makes the server's TLS certificate (RSA 2048, serverAuth), signed by ca, for names (host names
// and IP addresses devices connect by), and writes it and its key over any that are there.
func NewServerCert(ca CA, certPath, keyPath string, names []string) error {
	if len(names) == 0 {
		return errors.New("the server certificate needs at least one name or address")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	now := time.Now()
	notAfter := now.Add(ServerValidity)
	if ca.Cert.NotAfter.Before(notAfter) {
		notAfter = ca.Cert.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: names[0]},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              notAfter,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		// iOS requires serverAuth and subjectAltName on TLS server certificates.
		ExtKeyUsage:    []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		SubjectKeyId:   keyID(&key.PublicKey),
		AuthorityKeyId: ca.Cert.SubjectKeyId,
	}
	for _, name := range names {
		if ip := net.ParseIP(name); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, name)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return err
	}
	return writeKeyAndCert(keyPath, key, certPath, der)
}

// Missing is the names (host names and IP addresses) the certificate at certPath isn't valid for, so a device
// connecting by one of them would refuse the server.
func Missing(certPath string, names []string) ([]string, error) {
	data, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM certificate", certPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", certPath, err)
	}
	var missing []string
	for _, name := range names {
		if cert.VerifyHostname(name) != nil {
			missing = append(missing, name)
		}
	}
	return missing, nil
}

// LocalNames is what devices on the home network may reach this PC by: localhost, its host name and each private
// IPv4 address it has (192.168.x.x, 10.x.x.x, 172.16–31.x.x), in that order, each once.
func LocalNames() []string {
	names := []string{"localhost"}
	if host, err := os.Hostname(); err == nil && host != "" {
		names = append(names, strings.ToLower(host))
	}
	return append(names, PrivateAddresses()...)
}

// PrivateAddresses is this PC's private IPv4 addresses, of network interfaces that are up.
func PrivateAddresses() []string {
	var addrs []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		list, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range list {
			if ipnet, ok := a.(*net.IPNet); ok {
				if ip := ipnet.IP.To4(); ip != nil && ip.IsPrivate() && !slices.Contains(addrs, ip.String()) {
					addrs = append(addrs, ip.String())
				}
			}
		}
	}
	return addrs
}

func writeKeyAndCert(keyPath string, key *rsa.PrivateKey, certPath string, der []byte) error {
	for _, p := range []string{keyPath, certPath} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return err
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

// serial is a random 128-bit serial number.
func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		panic(err)
	}
	return n
}

// keyID is the SHA-1 of the public key, as OpenSSL's subjectKeyIdentifier=hash makes it.
func keyID(pub *rsa.PublicKey) []byte {
	sum := sha1.Sum(x509.MarshalPKCS1PublicKey(pub))
	return sum[:]
}
