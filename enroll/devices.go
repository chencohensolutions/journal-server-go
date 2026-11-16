package enroll

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Device is one certificate issued through enrollment.
type Device struct {
	Serial        string     `json:"serial"`
	User          string     `json:"user"`
	Name          string     `json:"name"`
	KeyProtection string     `json:"key_protection"`
	RegisteredAt  time.Time  `json:"registered_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	// The server keeps the files the user marked private from this device: they aren't in its change feed and
	// can't be fetched. Off unless set with `journal-server private <serial> withhold`.
	WithholdPrivate bool `json:"withhold_private,omitempty"`
	// The certificate this one renewed (renewCertificate): the same device with a new key. That one is revoked the
	// first time this one is used.
	RenewedFrom string `json:"renewed_from,omitempty"`
	// The app that enrolled (EnrollDeviceInput.app): "journal" when it said none. Two apps on one phone are two
	// devices, each with its own key and certificate.
	App string `json:"app,omitempty"`
	// The phone this certificate is one app of: the serial of the phone's first enrollment. Apps on one phone each
	// have their own key (Android ties a key to the app that made it), but the user enrolls the phone once: the
	// other apps enroll with a sibling code from an app already enrolled (SiblingCode), and join its phone. Empty
	// for the first: PhoneID is then its own serial.
	Phone string `json:"phone,omitempty"`
	// What an app made for sound recordings (SeesSoundRecordings) shows beside recordings: calls and voice messages,
	// each off until the user turns it on in the app (setSoundRecordingsShown).
	ShowCalls         bool `json:"show_calls,omitempty"`
	ShowVoiceMessages bool `json:"show_voice_messages,omitempty"`
}

// PhoneID is the phone the device belongs to: one id for all its apps, and all the certificates they renewed.
func (d Device) PhoneID() string {
	if d.Phone == "" {
		return d.Serial
	}
	return d.Phone
}

// The apps the server knows by name. Any other name is kept as it came, cleaned.
const (
	AppJournal   = "journal"
	AppAudioLogs = "audio-logs"
)

// AppName is the device's app: "journal" for a device enrolled before apps were named, or that named none.
func (d Device) AppName() string {
	if d.App == "" {
		return AppJournal
	}
	return d.App
}

// SeesSoundRecordings reports whether the device always sees the user's sound recordings, whatever the user hides
// (media.View.ShowAudio): an app made for them, Audio Logs, which uploads recordings and plays them back.
func (d Device) SeesSoundRecordings() bool { return d.AppName() == AppAudioLogs }

// WithholdsPrivate reports whether the server keeps the user's private files from the device: when set for its phone
// (WithholdPrivate), and always from Audio Logs, so a recording the user marked private never reaches a phone through
// it (the owner's call, Nov 16, 2026).
func (d Device) WithholdsPrivate() bool { return d.WithholdPrivate || d.AppName() == AppAudioLogs }

var ErrDeviceNotFound = errors.New("device not found")

// SerialString formats a certificate serial the way the registry stores it.
func SerialString(serial *big.Int) string {
	return fmt.Sprintf("%x", serial)
}

// Registry persists enrolled devices to a JSON file. Certificates that are not in the registry
// (e.g. made with scripts/generate-user-cert.ps1) are not affected by it.
type Registry struct {
	mu      sync.RWMutex
	path    string
	devices []Device
}

func OpenRegistry(path string) (*Registry, error) {
	r := &Registry{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &r.devices); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return r, nil
}

func (r *Registry) Add(device Device) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.devices = append(r.devices, device)
	if err := r.save(); err != nil {
		r.devices = r.devices[:len(r.devices)-1]
		return err
	}
	return nil
}

// List returns the devices of user (all users when empty), oldest first.
func (r *Registry) List(user string) []Device {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []Device
	for _, d := range r.devices {
		if user == "" || d.User == user {
			result = append(result, d)
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].RegisteredAt.Before(result[j].RegisteredAt) })
	return result
}

func (r *Registry) Revoke(serial string, at time.Time) (Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.devices {
		if r.devices[i].Serial != serial {
			continue
		}
		if r.devices[i].RevokedAt == nil {
			previous := r.devices[i]
			r.devices[i].RevokedAt = &at
			if err := r.save(); err != nil {
				r.devices[i] = previous
				return Device{}, err
			}
		}
		return r.devices[i], nil
	}
	return Device{}, ErrDeviceNotFound
}

// RevokePhone revokes the device with serial and every other certificate of its phone (its other apps, and what
// they renewed): the user enrolled the phone once, so it is revoked once. It returns the device with serial.
func (r *Registry) RevokePhone(serial string, at time.Time) (Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	target := -1
	for i := range r.devices {
		if r.devices[i].Serial == serial {
			target = i
		}
	}
	if target < 0 {
		return Device{}, ErrDeviceNotFound
	}
	previous := append([]Device(nil), r.devices...)
	phone := r.devices[target].PhoneID()
	for i := range r.devices {
		if r.devices[i].PhoneID() == phone && r.devices[i].RevokedAt == nil {
			r.devices[i].RevokedAt = &at
		}
	}
	if err := r.save(); err != nil {
		r.devices = previous
		return Device{}, err
	}
	return r.devices[target], nil
}

// SetSoundRecordingsShown sets what the device with serial shows of sound recordings beside recordings (calls, voice
// messages). Only this app of its phone changes.
func (r *Registry) SetSoundRecordingsShown(serial string, calls, voiceMessages bool) (Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.devices {
		if r.devices[i].Serial != serial {
			continue
		}
		if r.devices[i].ShowCalls == calls && r.devices[i].ShowVoiceMessages == voiceMessages {
			return r.devices[i], nil
		}
		previous := r.devices[i]
		r.devices[i].ShowCalls, r.devices[i].ShowVoiceMessages = calls, voiceMessages
		if err := r.save(); err != nil {
			r.devices[i] = previous
			return Device{}, err
		}
		return r.devices[i], nil
	}
	return Device{}, ErrDeviceNotFound
}

// SetWithholdPrivate turns withholding private files from a phone on or off: the device with serial and its
// phone's other apps alike.
func (r *Registry) SetWithholdPrivate(serial string, withhold bool) (Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	target := -1
	for i := range r.devices {
		if r.devices[i].Serial == serial {
			target = i
		}
	}
	if target < 0 {
		return Device{}, ErrDeviceNotFound
	}
	previous := append([]Device(nil), r.devices...)
	phone := r.devices[target].PhoneID()
	changed := false
	for i := range r.devices {
		if r.devices[i].PhoneID() == phone && r.devices[i].WithholdPrivate != withhold {
			r.devices[i].WithholdPrivate = withhold
			changed = true
		}
	}
	if changed {
		if err := r.save(); err != nil {
			r.devices = previous
			return Device{}, err
		}
	}
	return r.devices[target], nil
}

// Find returns the device with this certificate serial.
func (r *Registry) Find(serial string) (Device, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, d := range r.devices {
		if d.Serial == serial {
			return d, true
		}
	}
	return Device{}, false
}

// UseRenewal revokes the certificate the device with serial renewed, if it isn't yet: the new one is in use. It
// reports the old serial when it revoked one.
func (r *Registry) UseRenewal(serial string, at time.Time) (string, error) {
	r.mu.RLock()
	var from string
	for _, d := range r.devices {
		if d.Serial == serial {
			from = d.RenewedFrom
		}
	}
	revoked := true
	for _, d := range r.devices {
		if from != "" && d.Serial == from {
			revoked = d.RevokedAt != nil
		}
	}
	r.mu.RUnlock()
	if from == "" || revoked {
		return "", nil
	}
	if _, err := r.Revoke(from, at); err != nil {
		return "", err
	}
	return from, nil
}

func (r *Registry) IsRevoked(serial string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, d := range r.devices {
		if d.Serial == serial {
			return d.RevokedAt != nil
		}
	}
	return false
}

// save writes through a temp file and rename, so a crash never leaves a half-written registry.
// Callers hold mu.
func (r *Registry) save() error {
	data, err := json.MarshalIndent(r.devices, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.path), filepath.Base(r.path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), r.path)
}
