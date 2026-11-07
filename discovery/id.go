// Package discovery lets servers (and later phones) find a journal server whose address changes: a home line with
// no fixed IP and no host name (docs/discovery.md). The server is known by its id, the sha256 of its key, which
// every connection checks; an address is only a hint. Each server publishes its addresses on the BitTorrent DHT
// under a key of its own, signed and sealed, and the others look them up there when the addresses they know stop
// answering. Nothing is relayed: the server must still be reachable at one of the addresses it publishes.
package discovery

import (
	"encoding/base32"
	"encoding/hex"
	"errors"
	"strings"
)

// A server id is written as Syncthing writes device ids: the 32 bytes in base32 (52 letters), a check letter after
// each 13 (Luhn mod 32, Syncthing's own variant), in 8 groups of 7, "MFZWI3D-BONSGYC-…". A typo is found before
// anything is dialed.

const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"

// FormatID writes a fingerprint (peers.Fingerprint: 64 hex digits) as a server id; "" for one that isn't.
func FormatID(fingerprint string) string {
	raw, err := hex.DecodeString(fingerprint)
	if err != nil || len(raw) != 32 {
		return ""
	}
	plain := strings.TrimRight(base32.StdEncoding.EncodeToString(raw), "=") // 52 letters
	var checked strings.Builder
	for i := 0; i < 4; i++ {
		part := plain[i*13 : (i+1)*13]
		checked.WriteString(part)
		checked.WriteByte(luhn32(part))
	}
	s := checked.String() // 56 letters
	groups := make([]string, 8)
	for i := range groups {
		groups[i] = s[i*7 : (i+1)*7]
	}
	return strings.Join(groups, "-")
}

// ParseID reads a server id (any case, dashes or spaces anywhere) or a fingerprint in hex, and gives the
// fingerprint in lower-case hex.
func ParseID(s string) (string, error) {
	s = strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if len(s) == 64 {
		if raw, err := hex.DecodeString(s); err == nil && len(raw) == 32 {
			return strings.ToLower(s), nil
		}
	}
	s = strings.ToUpper(s)
	if len(s) != 56 {
		return "", errors.New("a server id has 56 letters and digits (8 groups of 7)")
	}
	var plain strings.Builder
	for i := 0; i < 4; i++ {
		part, check := s[i*14:i*14+13], s[i*14+13]
		if strings.Trim(part, alphabet) != "" {
			return "", errors.New("a server id has only the letters A–Z and the digits 2–7")
		}
		if luhn32(part) != check {
			return "", errors.New("the server id has a typo: a check letter doesn't match")
		}
		plain.WriteString(part)
	}
	raw, err := base32.StdEncoding.DecodeString(plain.String() + "====")
	if err != nil || len(raw) != 32 {
		return "", errors.New("not a server id")
	}
	return hex.EncodeToString(raw), nil
}

// luhn32 is the check letter of s, as Syncthing's luhnBase32 makes it.
func luhn32(s string) byte {
	const n = 32
	factor, sum := 1, 0
	for i := 0; i < len(s); i++ {
		addend := factor * strings.IndexByte(alphabet, s[i])
		if factor == 2 {
			factor = 1
		} else {
			factor = 2
		}
		sum += addend/n + addend%n
	}
	return alphabet[(n-sum%n)%n]
}
