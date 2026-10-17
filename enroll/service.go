// Package enroll lets a device trade a one-time enrollment code and a certificate request for a
// client certificate signed by the server's client CA. See docs/authentication.md.
package enroll

import (
	"strings"
	"time"
	"unicode/utf8"
)

// Rejection is an expected enrollment failure, reported to the client as data.
type Rejection string

const (
	// RejectInvalidCode covers wrong, expired and already used codes alike, so guessing learns nothing.
	RejectInvalidCode Rejection = "INVALID_CODE"
	RejectInvalidCSR  Rejection = "INVALID_CSR"
	RejectRateLimited Rejection = "RATE_LIMITED"
)

const maxDeviceNameLength = 64

type Service struct {
	Codes    *CodeStore
	Issuer   *Issuer
	Registry *Registry
	limiter  *failureLimiter
	now      func() time.Time
}

func NewService(issuer *Issuer, registry *Registry, now func() time.Time) *Service {
	return &Service{
		Codes:    NewCodeStore(now),
		Issuer:   issuer,
		Registry: registry,
		limiter:  newFailureLimiter(now),
		now:      now,
	}
}

type Preview struct {
	User      string
	ExpiresAt time.Time
}

// Preview tells the device which account a code belongs to without using it up.
// Wrong codes count against the client's failure limit, like in Enroll.
func (s *Service) Preview(clientIP, code string) (Preview, Rejection) {
	if !s.limiter.Allow(clientIP) {
		return Preview{}, RejectRateLimited
	}
	pending, ok := s.Codes.Peek(code)
	if !ok {
		s.limiter.Fail(clientIP)
		return Preview{}, RejectInvalidCode
	}
	return Preview{User: pending.User, ExpiresAt: pending.ExpiresAt}, ""
}

type Request struct {
	Code          string
	DeviceName    string
	CSR           []byte // PKCS#10, DER
	KeyProtection string
}

type Enrolled struct {
	Device      Device
	Certificate []byte // DER
}

// Enroll redeems the code and issues a certificate for the request's key. The code is only used up
// once the request itself checked out, so a malformed request does not waste it.
func (s *Service) Enroll(clientIP string, req Request) (Enrolled, Rejection, error) {
	if !s.limiter.Allow(clientIP) {
		return Enrolled{}, RejectRateLimited, nil
	}
	csr, err := ParseDeviceCSR(req.CSR)
	if err != nil {
		return Enrolled{}, RejectInvalidCSR, nil
	}
	pending, ok := s.Codes.Redeem(req.Code)
	if !ok {
		s.limiter.Fail(clientIP)
		return Enrolled{}, RejectInvalidCode, nil
	}

	cert, err := s.Issuer.Issue(csr, pending.User)
	if err != nil {
		return Enrolled{}, "", err
	}
	device := Device{
		Serial:        SerialString(cert.SerialNumber),
		User:          pending.User,
		Name:          cleanDeviceName(req.DeviceName),
		KeyProtection: req.KeyProtection,
		RegisteredAt:  s.now().UTC(),
		ExpiresAt:     cert.NotAfter.UTC(),
	}
	if err := s.Registry.Add(device); err != nil {
		return Enrolled{}, "", err
	}
	return Enrolled{Device: device, Certificate: cert.Raw}, "", nil
}

func cleanDeviceName(name string) string {
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < ' ' {
			return -1
		}
		return r
	}, name))
	if name == "" {
		return "Unnamed device"
	}
	for utf8.RuneCountInString(name) > maxDeviceNameLength {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}
