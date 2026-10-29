// Package peers lets another journal server keep its users' copies on this one (docs/peers.md): the host side of a
// swap. The host's admin invites a peer (a one-time code), the peer enrolls as a device does and gets a client
// certificate marked enroll.PeerUnit, and then stores and reads files in its own space under peers_folder through
// the peer API (/peer/). A peer only stores: this server doesn't index, preview or serve its files to phones.
package peers

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Peer is a server that keeps copies here.
type Peer struct {
	// Named by the invite; also its certificate's CN and its folder under peers_folder.
	Name   string `json:"name"`
	Serial string `json:"serial"`
	// Most bytes its files may take here.
	Quota     int64      `json:"quota"`
	AddedAt   time.Time  `json:"added_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// ErrPeerNotFound is a name no peer has.
var ErrPeerNotFound = errors.New("no such peer")

// Registry keeps the peers in peers.json (beside devices.json).
type Registry struct {
	mu    sync.RWMutex
	path  string
	peers []Peer
}

// OpenRegistry reads path; a missing file is no peers yet.
func OpenRegistry(path string) (*Registry, error) {
	r := &Registry{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	var file struct {
		V     int    `json:"v"`
		Peers []Peer `json:"peers"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if file.V != 1 {
		return nil, fmt.Errorf("%s: version %d isn't known", path, file.V)
	}
	r.peers = file.Peers
	return r, nil
}

// List is every peer, revoked ones too, by name and then oldest first.
func (r *Registry) List() []Peer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := append([]Peer(nil), r.peers...)
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Name != list[j].Name {
			return list[i].Name < list[j].Name
		}
		return list[i].AddedAt.Before(list[j].AddedAt)
	})
	return list
}

// Active is the peer of that name whose certificate isn't revoked.
func (r *Registry) Active(name string) (Peer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.peers {
		if p.Name == name && p.RevokedAt == nil {
			return p, true
		}
	}
	return Peer{}, false
}

// BySerial is the peer with this certificate serial, revoked or not.
func (r *Registry) BySerial(serial string) (Peer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.peers {
		if p.Serial == serial {
			return p, true
		}
	}
	return Peer{}, false
}

func (r *Registry) add(p Peer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.peers = append(r.peers, p)
	if err := r.save(); err != nil {
		r.peers = r.peers[:len(r.peers)-1]
		return err
	}
	return nil
}

// Revoke refuses the active certificate of the peer named; its files stay.
func (r *Registry) Revoke(name string, at time.Time) (Peer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.peers {
		if r.peers[i].Name != name || r.peers[i].RevokedAt != nil {
			continue
		}
		r.peers[i].RevokedAt = &at
		if err := r.save(); err != nil {
			r.peers[i].RevokedAt = nil
			return Peer{}, err
		}
		return r.peers[i], nil
	}
	return Peer{}, ErrPeerNotFound
}

// save writes through a temp file and rename. Callers hold mu.
func (r *Registry) save() error {
	data, err := json.MarshalIndent(map[string]any{"v": 1, "peers": r.peers}, "", "  ")
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
