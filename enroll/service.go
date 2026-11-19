// Package enroll lets a device trade a one-time enrollment code and a certificate request for a
// client certificate signed by the server's client CA. See docs/authentication.md.
package enroll

import (
	"strings"
	"sync"
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
	// A renewal from a certificate the registry doesn't know (made outside enrollment), or revoked.
	RejectNotEnrolled Rejection = "NOT_ENROLLED"
	// A sibling code used by another app than the one it was made for.
	RejectWrongApp Rejection = "WRONG_APP"
)

// RenewBefore is how long before its certificate expires a device renews it.
var RenewBefore = 30 * 24 * time.Hour

// renewEarliest is how long before its certificate expires the server renews it at the earliest: twice the window the
// apps renew in, for a clock that is off. Earlier, a renewal is refused (RATE_LIMITED), so a stolen certificate can't
// be made to last for ever by renewing it.
var renewEarliest = 2 * RenewBefore

// maxRenewals is how many certificates one certificate may be renewed to: a renewal that failed part way is tried
// again (the app keeps the old one until the new one works), but no more often than this.
const maxRenewals = 5

// Sibling codes a phone may ask for in siblingWindow: a code is asked when the user confirms in the app, a few times
// at most.
const (
	maxSiblingCodes = 10
	siblingWindow   = time.Hour
)

const maxDeviceNameLength = 64

type Service struct {
	Codes    *CodeStore
	Issuer   *Issuer
	Registry *Registry
	limiter  *failureLimiter
	now      func() time.Time

	mu       sync.Mutex
	siblings map[string][]time.Time // sibling codes asked, by phone
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

// SiblingCode makes a code for another app on the phone of the enrolled device with serial, so the user enrolls a
// phone once: the app already enrolled asks for it (the user confirms there) and hands it to the other app on the
// phone, which enrolls with it as usual and joins the phone (Device.Phone).
// It returns the code, the app's name as the server keeps it ("journal" for none) and when the code expires.
func (s *Service) SiblingCode(serial, app string) (string, string, time.Time, Rejection) {
	parent, ok := s.Registry.Find(serial)
	if !ok || parent.RevokedAt != nil {
		return "", "", time.Time{}, RejectNotEnrolled
	}
	if !s.allowSibling(parent.PhoneID()) {
		return "", "", time.Time{}, RejectRateLimited
	}
	clean := cleanAppName(app)
	code, expiresAt := s.Codes.CreateSibling(parent.User, parent.PhoneID(), clean)
	return code, Device{App: clean}.AppName(), expiresAt, ""
}

// allowSibling counts a sibling code for phone, and reports whether it is within maxSiblingCodes in siblingWindow.
func (s *Service) allowSibling(phone string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.siblings == nil {
		s.siblings = map[string][]time.Time{}
	}
	now := s.now()
	var recent []time.Time
	for _, t := range s.siblings[phone] {
		if now.Sub(t) < siblingWindow {
			recent = append(recent, t)
		}
	}
	if len(recent) >= maxSiblingCodes {
		s.siblings[phone] = recent
		return false
	}
	s.siblings[phone] = append(recent, now)
	return true
}

type Request struct {
	Code          string
	DeviceName    string
	CSR           []byte // PKCS#10, DER
	KeyProtection string
	// The app enrolling ("journal", "audio-logs"); empty for journal.
	App string
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
	pending, ok := s.Codes.Peek(req.Code)
	if ok && pending.Phone != "" && pending.App != cleanAppName(req.App) {
		return Enrolled{}, RejectWrongApp, nil
	}
	pending, ok = s.Codes.Redeem(req.Code)
	if !ok {
		s.limiter.Fail(clientIP)
		return Enrolled{}, RejectInvalidCode, nil
	}
	name := cleanDeviceName(req.DeviceName)
	withhold := false
	if pending.Phone != "" {
		// One phone, one name (the one it enrolled with first), and the phone's choices.
		if phone, found := s.Registry.Find(pending.Phone); found {
			name = phone.Name
			withhold = phone.WithholdPrivate
		}
	}

	cert, err := s.Issuer.Issue(csr, pending.User)
	if err != nil {
		return Enrolled{}, "", err
	}
	device := Device{
		Serial:        SerialString(cert.SerialNumber),
		User:          pending.User,
		Name:          name,
		KeyProtection: req.KeyProtection,
		RegisteredAt:  s.now().UTC(),
		ExpiresAt:     cert.NotAfter.UTC(),
		App:           cleanAppName(req.App),
		Phone:         pending.Phone,
		// Withholding private files is set per phone (Registry.SetWithholdPrivate).
		WithholdPrivate: withhold,
	}
	if err := s.Registry.Add(device); err != nil {
		return Enrolled{}, "", err
	}
	return Enrolled{Device: device, Certificate: cert.Raw}, "", nil
}

// Renew issues a new certificate for the enrolled device with serial, for the request's key (a new one, made by the
// device): the same user and name, and the device's choices (WithholdPrivate) carried over. The old certificate
// works until the new one is first used (Registry.UseRenewal).
func (s *Service) Renew(serial string, csrDER []byte, keyProtection string) (Enrolled, Rejection, error) {
	old, ok := s.Registry.Find(serial)
	if !ok || old.RevokedAt != nil {
		return Enrolled{}, RejectNotEnrolled, nil
	}
	if !old.ExpiresAt.IsZero() && old.ExpiresAt.Sub(s.now()) > renewEarliest {
		return Enrolled{}, RejectRateLimited, nil
	}
	renewed := 0
	for _, d := range s.Registry.List(old.User) {
		if d.RenewedFrom == serial {
			renewed++
		}
	}
	if renewed >= maxRenewals {
		return Enrolled{}, RejectRateLimited, nil
	}
	csr, err := ParseDeviceCSR(csrDER)
	if err != nil {
		return Enrolled{}, RejectInvalidCSR, nil
	}
	cert, err := s.Issuer.Issue(csr, old.User)
	if err != nil {
		return Enrolled{}, "", err
	}
	if keyProtection == "" {
		keyProtection = old.KeyProtection
	}
	device := Device{
		Serial:            SerialString(cert.SerialNumber),
		User:              old.User,
		Name:              old.Name,
		KeyProtection:     keyProtection,
		RegisteredAt:      s.now().UTC(),
		ExpiresAt:         cert.NotAfter.UTC(),
		WithholdPrivate:   old.WithholdPrivate,
		RenewedFrom:       old.Serial,
		App:               old.App,
		Phone:             old.PhoneID(),
		ShowCalls:         old.ShowCalls,
		ShowVoiceMessages: old.ShowVoiceMessages,
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

// cleanAppName keeps an app's name to lower-case letters, digits and dashes, at most 32 of them; "" (journal) when
// nothing is left, or when it names journal.
func cleanAppName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if b.Len() >= 32 {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.String() == AppJournal {
		return ""
	}
	return b.String()
}
