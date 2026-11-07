package discovery

import (
	"crypto"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

// The DHT keeps a record under an ed25519 key (BEP 44: mutable items), which TLS keys aren't. A server's DHT key is
// made from its TLS key, so it needs no file of its own and changes only when the server's key (and id) does. The
// public half travels to peers (and later phones) when they enroll and on GET /peer/addresses; it is what they look
// up.

// salt keeps journal records apart from anything else stored under the same key (BEP 44 "salt").
var salt = []byte("journal-server/v1")

// maxValue is the most a BEP 44 value may take, bencoded.
const maxValue = 1000

// KeyFor is the server's DHT key, made from its TLS key.
func KeyFor(tlsKey crypto.PrivateKey) (ed25519.PrivateKey, error) {
	der, err := x509.MarshalPKCS8PrivateKey(tlsKey)
	if err != nil {
		return nil, err
	}
	seed, err := hkdf.Key(sha256.New, der, nil, "journal dht key v1", ed25519.SeedSize)
	if err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// ParseKey reads a DHT public key in hex, as peer.json keeps it.
func ParseKey(s string) (ed25519.PublicKey, error) {
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("not a DHT key")
	}
	return raw, nil
}

// Record is what a server publishes: where it can be reached now.
type Record struct {
	V int `json:"v"`
	// "https://host:port", the most likely first.
	Addresses []string  `json:"a"`
	At        time.Time `json:"t"`
}

// The record is sealed with a key made from the server's fingerprint, so the nodes that keep it and anyone crawling
// the DHT see an opaque blob, not a home IP; only who knows the server (its peers, its phones) can open it. The
// signature (by the DHT key) says who wrote it, and the TLS check on dialing says who answers.

func sealKey(fingerprint string) ([]byte, error) {
	raw, err := hex.DecodeString(fingerprint)
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("%q isn't a fingerprint", fingerprint)
	}
	return hkdf.Key(sha256.New, raw, nil, "journal dht seal v1", chacha20poly1305.KeySize)
}

// seal is r, sealed for those who know fingerprint, with as many of its addresses as fit in a DHT value.
func seal(r Record, fingerprint string) ([]byte, error) {
	key, err := sealKey(fingerprint)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	for {
		plain, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		// Bencoded, the value is "<length>:" and the bytes: 4 bytes of length is plenty below 1000.
		if size := 4 + aead.NonceSize() + len(plain) + aead.Overhead(); size <= maxValue || len(r.Addresses) == 0 {
			nonce := make([]byte, aead.NonceSize(), size)
			rand.Read(nonce)
			return aead.Seal(nonce, nonce, plain, salt), nil
		}
		r.Addresses = r.Addresses[:len(r.Addresses)-1]
	}
}

// open reads a sealed record.
func open(sealed []byte, fingerprint string) (Record, error) {
	key, err := sealKey(fingerprint)
	if err != nil {
		return Record{}, err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return Record{}, err
	}
	if len(sealed) < aead.NonceSize() {
		return Record{}, errors.New("the record is too short")
	}
	plain, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], salt)
	if err != nil {
		return Record{}, errors.New("the record doesn't open with this server's id")
	}
	var r Record
	if err := json.Unmarshal(plain, &r); err != nil || r.V != 1 {
		return Record{}, fmt.Errorf("the record isn't readable: %v", err)
	}
	return r, nil
}
