package enroll

import (
	"crypto/rand"
	"crypto/sha256"
	"strings"
	"sync"
	"time"
)

// Crockford-style alphabet: 32 symbols without I, L, O and U, so 0/O and 1/I/L cannot be confused.
const codeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const (
	codeLength = 8
	// CodeTTL is how long an enrollment code stays valid.
	CodeTTL = 10 * time.Minute
)

type pendingCode struct {
	User      string
	ExpiresAt time.Time
}

// CodeStore holds one-time enrollment codes in memory, keyed by the SHA-256 of the normalized code
// so the codes themselves are never kept. A server restart drops all pending codes.
type CodeStore struct {
	mu    sync.Mutex
	codes map[[32]byte]pendingCode
	now   func() time.Time
}

func NewCodeStore(now func() time.Time) *CodeStore {
	return &CodeStore{codes: map[[32]byte]pendingCode{}, now: now}
}

// Create returns a new code for user, formatted for display ("K7QM-4XPD").
func (s *CodeStore) Create(user string) (string, time.Time) {
	raw := make([]byte, codeLength)
	rand.Read(raw)
	for i := range raw {
		raw[i] = codeAlphabet[raw[i]&31] // 256 is a multiple of 32: no modulo bias
	}
	code := string(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeExpired()
	expiresAt := s.now().Add(CodeTTL)
	s.codes[sha256.Sum256([]byte(code))] = pendingCode{User: user, ExpiresAt: expiresAt}
	return code[:4] + "-" + code[4:], expiresAt
}

// Peek reports the pending code without using it up.
func (s *CodeStore) Peek(code string) (pendingCode, bool) {
	key, ok := codeKey(code)
	if !ok {
		return pendingCode{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeExpired()
	pending, ok := s.codes[key]
	return pending, ok
}

// Redeem uses up the code. Lookup and removal happen under one lock, so a code works only once
// even when two requests race.
func (s *CodeStore) Redeem(code string) (pendingCode, bool) {
	key, ok := codeKey(code)
	if !ok {
		return pendingCode{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeExpired()
	pending, ok := s.codes[key]
	if ok {
		delete(s.codes, key)
	}
	return pending, ok
}

func (s *CodeStore) removeExpired() {
	now := s.now()
	for key, pending := range s.codes {
		if !now.Before(pending.ExpiresAt) {
			delete(s.codes, key)
		}
	}
}

// codeKey normalizes what the user typed (case, dashes, spaces, look-alike letters) and hashes it.
func codeKey(input string) ([32]byte, bool) {
	normalized := strings.Map(func(r rune) rune {
		switch r {
		case '-', ' ':
			return -1
		case 'O', 'o':
			return '0'
		case 'I', 'i', 'L', 'l':
			return '1'
		}
		if r >= 'a' && r <= 'z' {
			return r - 'a' + 'A'
		}
		return r
	}, input)
	if len(normalized) != codeLength || strings.Trim(normalized, codeAlphabet) != "" {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(normalized)), true
}
