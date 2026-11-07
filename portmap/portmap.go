// Package portmap asks the home router to forward the server's port to this machine (UPnP IGD, or NAT-PMP), so a
// friend's server or a phone on the internet can reach it without a port forwarded by hand (docs/peers.md,
// "Opening the router"). The mapping is asked for at start, renewed before its lease ends, and removed when the
// server stops. The router's outside address joins the addresses the server publishes.
package portmap

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/netip"
	"sync"
	"time"
)

// Gateway is a router that maps ports: a UPnP IGD service or a NAT-PMP router.
type Gateway interface {
	// Kind names it in the log: "UPnP" or "NAT-PMP".
	Kind() string
	// Map forwards external TCP port to internal on this machine for lease, and gives the external port granted.
	Map(ctx context.Context, internal, external uint16, lease time.Duration, description string) (uint16, error)
	// Unmap removes the mapping of external to internal.
	Unmap(ctx context.Context, internal, external uint16) error
	// ExternalIP is the router's outside address.
	ExternalIP(ctx context.Context) (net.IP, error)
}

// Mapping is the router's forwarding of the server's port, as last granted.
type Mapping struct {
	Kind         string
	ExternalIP   net.IP
	ExternalPort uint16
	// The router's outside address isn't public (carrier-grade NAT, or a second router in front): the mapping
	// works only up to there, and the internet still can't reach the server through it.
	Private bool
}

// Mapper keeps the server's port mapped while it runs.
type Mapper struct {
	Port        uint16
	Description string
	// Finds the routers on the network; nil finds them with UPnP and NAT-PMP (Find).
	Find func(ctx context.Context) []Gateway

	mu      sync.Mutex
	current *Mapping
}

var (
	// Lease is how long a mapping is asked for; it is renewed at half of it.
	Lease = time.Hour
	// RetryEvery is how often a server without a mapping asks again (a router restarted, UPnP turned on).
	RetryEvery = 10 * time.Minute
)

// Current is the mapping as last granted, or nil.
func (m *Mapper) Current() *Mapping {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil {
		return nil
	}
	c := *m.current
	return &c
}

// Run maps the port, renews it, and removes it when ctx ends.
func (m *Mapper) Run(ctx context.Context) {
	find := m.Find
	if find == nil {
		find = Find
	}
	var gw Gateway
	warned := false
	for {
		wait := RetryEvery
		if gw != nil {
			if mapping, err := m.mapOn(ctx, gw); err == nil {
				m.report(mapping)
				wait = Lease / 2
			} else {
				if ctx.Err() == nil {
					log.Printf("Port mapping: renewing on %s: %v; looking for the router again", gw.Kind(), err)
				}
				m.set(nil)
				gw, wait = nil, time.Minute
				if wait > RetryEvery {
					wait = RetryEvery
				}
			}
		} else {
			gw = m.first(ctx, find)
			switch {
			case gw != nil:
				wait = Lease / 2
			case ctx.Err() == nil && !warned:
				warned = true
				log.Printf("Port mapping: no router on this network maps ports (UPnP and NAT-PMP off, or none answers); "+
					"forward port %d by hand, or use IPv6. Asked again every %s.", m.Port, RetryEvery)
			}
		}
		select {
		case <-ctx.Done():
			m.remove(gw)
			return
		case <-time.After(wait):
		}
	}
}

// first maps the port on the first of the routers found that agrees, and gives it.
func (m *Mapper) first(ctx context.Context, find func(context.Context) []Gateway) Gateway {
	ask, cancel := context.WithTimeout(ctx, 10*time.Second)
	gws := find(ask)
	cancel()
	for _, g := range gws {
		mapping, err := m.mapOn(ctx, g)
		if err == nil {
			m.report(mapping)
			return g
		}
		if ctx.Err() != nil {
			return nil
		}
		log.Printf("Port mapping: %s refused port %d: %v", g.Kind(), m.Port, err)
	}
	return nil
}

// remove takes the mapping off gw when the server stops, so the router doesn't keep forwarding to nothing.
func (m *Mapper) remove(gw Gateway) {
	c := m.Current()
	if gw == nil || c == nil {
		return
	}
	stop, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := gw.Unmap(stop, m.Port, c.ExternalPort); err == nil {
		log.Printf("Port mapping: removed port %d from the router", c.ExternalPort)
	}
	m.set(nil)
}

func (m *Mapper) mapOn(ctx context.Context, g Gateway) (*Mapping, error) {
	ask, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	external := m.Port
	if c := m.Current(); c != nil {
		external = c.ExternalPort
	}
	got, err := g.Map(ask, m.Port, external, Lease, m.Description)
	if err != nil {
		return nil, err
	}
	ip, err := g.ExternalIP(ask)
	if err != nil {
		return nil, fmt.Errorf("mapped, but the router didn't say its outside address: %w", err)
	}
	return &Mapping{Kind: g.Kind(), ExternalIP: ip, ExternalPort: got, Private: !Public(ip)}, nil
}

// report keeps mapping, and logs it when it changed.
func (m *Mapper) report(mapping *Mapping) {
	before := m.Current()
	m.set(mapping)
	if before != nil && before.ExternalIP.Equal(mapping.ExternalIP) && before.ExternalPort == mapping.ExternalPort {
		return
	}
	at := net.JoinHostPort(mapping.ExternalIP.String(), fmt.Sprint(mapping.ExternalPort))
	if mapping.Private {
		log.Printf("Port mapping: the router (%s) forwards %s to this server, but %s isn't a public address: the "+
			"internet can't reach the server through it (carrier-grade NAT, or another router in front). Ask the ISP "+
			"for a public IPv4 address, or use IPv6.", mapping.Kind, at, mapping.ExternalIP)
		return
	}
	log.Printf("Port mapping: the router (%s) forwards %s to this server", mapping.Kind, at)
}

func (m *Mapper) set(mapping *Mapping) {
	m.mu.Lock()
	m.current = mapping
	m.mu.Unlock()
}

// Public is an address the internet can reach: not private, shared (100.64.0.0/10), loopback or link-local.
func Public(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	return a.IsGlobalUnicast() && !a.IsPrivate() && !netip.MustParsePrefix("100.64.0.0/10").Contains(a)
}

// URL is the server's address on the internet through the mapping, or "" when the internet can't reach it there.
func (m *Mapping) URL() string {
	if m == nil || m.Private {
		return ""
	}
	return "https://" + net.JoinHostPort(m.ExternalIP.String(), fmt.Sprint(m.ExternalPort))
}
