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
}

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
