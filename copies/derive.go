package copies

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"filippo.io/age"
	"golang.org/x/crypto/argon2"
)

// A copy key can come from a passphrase instead of a key file (copy-key new --from-passphrase): the age key and the
// naming key are derived from it with Argon2id and a random salt. The salt isn't secret: this server keeps it, and
// so does each encrypted copy (copy-key.json at its root), so the passphrase alone brings the copy back. Whoever
// holds the copy can try passphrases against it, as fast as Argon2id lets them: only a long passphrase is accepted.

// Derivation is how a copy key is derived from its passphrase.
type Derivation struct {
	Salt      []byte `json:"salt"`
	Time      uint32 `json:"time"`
	MemoryKiB uint32 `json:"memoryKiB"`
	Threads   uint8  `json:"threads"`
}

// DefaultDerivation is Argon2id with 256 MiB, 3 passes and 4 lanes: about a second on a home server.
var DefaultDerivation = Derivation{Time: 3, MemoryKiB: 256 << 10, Threads: 4}

// derivationName is the salt and parameters beside an encrypted copy made with a derived key.
const derivationName = "copy-key.json"

// NewDerivation is DefaultDerivation with a new random salt.
func NewDerivation() Derivation {
	d := DefaultDerivation
	d.Salt = make([]byte, 16)
	rand.Read(d.Salt)
	return d
}

// WeakPassphrase says why a passphrase is too weak for a copy key, or "": at least 24 characters, or at least 20 in
// 4 words or more. Six random words (diceware) are best.
func WeakPassphrase(p string) string {
	p = strings.TrimSpace(p)
	words := len(strings.FieldsFunc(p, func(r rune) bool { return unicode.IsSpace(r) || r == '-' }))
	length := len([]rune(p))
	if length >= 24 || (length >= 20 && words >= 4) {
		return ""
	}
	return fmt.Sprintf("%d characters in %d words: use at least 24 characters, or 4 words or more and 20 characters "+
		"(six random words are best)", length, words)
}

// DeriveKey is the copy key of user that passphrase gives with d, and its age identity.
func DeriveKey(user, passphrase string, d Derivation, now time.Time) (CopyKey, *age.X25519Identity, error) {
	if len(d.Salt) < 16 || d.Time == 0 || d.MemoryKiB == 0 || d.Threads == 0 {
		return CopyKey{}, nil, errors.New("the key's derivation isn't complete")
	}
	material := argon2.IDKey([]byte(passphrase), d.Salt, d.Time, d.MemoryKiB, d.Threads, 64)
	identity, err := age.ParseX25519Identity(strings.ToUpper(bech32Encode("age-secret-key-", material[:32])))
	if err != nil {
		return CopyKey{}, nil, err
	}
	derived := d
	key := CopyKey{V: 1, User: user, Recipient: identity.Recipient().String(), NamingKey: material[32:],
		CreatedAt: now.UTC(), Derived: &derived}
	return key, identity, nil
}

// bech32Encode is BIP 173's encoding, as age writes its keys.
func bech32Encode(hrp string, data []byte) string {
	const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
	// 8-bit bytes to 5-bit groups.
	var values []byte
	acc, bits := 0, 0
	for _, b := range data {
		acc = acc<<8 | int(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			values = append(values, byte(acc>>bits&31))
		}
	}
	if bits > 0 {
		values = append(values, byte(acc<<(5-bits)&31))
	}
	polymod := func(values []byte) int {
		gen := []int{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
		chk := 1
		for _, v := range values {
			top := chk >> 25
			chk = (chk&0x1ffffff)<<5 ^ int(v)
			for i := range 5 {
				if top>>i&1 == 1 {
					chk ^= gen[i]
				}
			}
		}
		return chk
	}
	var expanded []byte
	for _, c := range hrp {
		expanded = append(expanded, byte(c>>5))
	}
	expanded = append(expanded, 0)
	for _, c := range hrp {
		expanded = append(expanded, byte(c&31))
	}
	mod := polymod(append(append(expanded, values...), 0, 0, 0, 0, 0, 0)) ^ 1
	var out strings.Builder
	out.WriteString(hrp)
	out.WriteByte('1')
	for _, v := range values {
		out.WriteByte(charset[v])
	}
	for i := range 6 {
		out.WriteByte(charset[mod>>(5*(5-i))&31])
	}
	return out.String()
}
