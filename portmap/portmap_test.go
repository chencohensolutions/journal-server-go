package portmap

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeGateway is a router: it maps what it is asked unless refuse is set, and counts the calls.
type fakeGateway struct {
	kind     string
	ip       net.IP
	external uint16 // the port it grants; 0 grants the one asked for

	mu       sync.Mutex
	refuse   bool
	taken    map[uint16]bool // outside ports another machine holds
	maps     int
	unmapped []uint16
}

func (g *fakeGateway) Kind() string { return g.kind }

func (g *fakeGateway) Map(_ context.Context, internal, external uint16, _ time.Duration, _ string) (uint16, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refuse || g.taken[external] {
		return 0, errors.New("refused")
	}
	g.maps++
	if g.external != 0 {
		return g.external, nil
	}
	return external, nil
}

func (g *fakeGateway) Unmap(_ context.Context, internal, external uint16) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.unmapped = append(g.unmapped, external)
	return nil
}

func (g *fakeGateway) ExternalIP(context.Context) (net.IP, error) { return g.ip, nil }

func (g *fakeGateway) counts() (int, []uint16) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.maps, append([]uint16(nil), g.unmapped...)
}

func shortTimes(t *testing.T) {
	lease, retry := Lease, RetryEvery
	Lease, RetryEvery = 40*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { Lease, RetryEvery = lease, retry })
}

// run runs m until the test ends, and waits for it to stop before the times are put back.
func run(t *testing.T, m *Mapper) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !ok(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMapsRenewsAndRemovesOnStop(t *testing.T) {
	shortTimes(t)
	refusing := &fakeGateway{kind: "UPnP", ip: net.IPv4(203, 0, 113, 7), refuse: true}
	router := &fakeGateway{kind: "NAT-PMP", ip: net.IPv4(203, 0, 113, 7), external: 40123}
	m := &Mapper{Port: 56398, Find: func(context.Context) []Gateway { return []Gateway{refusing, router} }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()

	waitFor(t, "a mapping", func() bool { return m.Current() != nil })
	c := m.Current()
	if c.Kind != "NAT-PMP" || c.ExternalPort != 40123 || c.Private || c.URL() != "https://203.0.113.7:40123" {
		t.Fatalf("mapping: %+v (%s)", c, c.URL())
	}
	waitFor(t, "renewals", func() bool { n, _ := router.counts(); return n >= 3 })

	cancel()
	<-done
	if _, unmapped := router.counts(); len(unmapped) != 1 || unmapped[0] != 40123 {
		t.Fatalf("unmapped on stop: %v", unmapped)
	}
	if _, unmapped := refusing.counts(); len(unmapped) != 0 {
		t.Fatalf("the router that refused was asked to unmap: %v", unmapped)
	}
	if m.Current() != nil {
		t.Fatalf("still a mapping after stop: %+v", m.Current())
	}
}

func TestFindsTheRouterLaterAndAgainAfterItForgets(t *testing.T) {
	shortTimes(t)
	router := &fakeGateway{kind: "UPnP", ip: net.IPv4(198, 51, 100, 2)}
	var mu sync.Mutex
	present := false
	m := &Mapper{Port: 56398, Find: func(context.Context) []Gateway {
		mu.Lock()
		defer mu.Unlock()
		if !present {
			return nil
		}
		return []Gateway{router}
	}}
	run(t, m)

	time.Sleep(50 * time.Millisecond)
	if m.Current() != nil {
		t.Fatal("a mapping with no router")
	}
	mu.Lock()
	present = true // UPnP turned on, or the router came back
	mu.Unlock()
	waitFor(t, "a mapping once the router answers", func() bool { return m.Current() != nil })

	router.mu.Lock()
	router.refuse = true // the router restarted and refuses the renewal
	router.mu.Unlock()
	waitFor(t, "the mapping dropped", func() bool { return m.Current() == nil })
	router.mu.Lock()
	router.refuse = false
	router.mu.Unlock()
	waitFor(t, "a mapping again", func() bool { return m.Current() != nil })
}

func TestPrivateOutsideAddress(t *testing.T) {
	shortTimes(t)
	router := &fakeGateway{kind: "UPnP", ip: net.IPv4(100, 72, 1, 9)} // carrier-grade NAT
	m := &Mapper{Port: 56398, Find: func(context.Context) []Gateway { return []Gateway{router} }}
	run(t, m)
	waitFor(t, "a mapping", func() bool { return m.Current() != nil })
	if c := m.Current(); !c.Private || c.URL() != "" {
		t.Fatalf("a mapping behind carrier-grade NAT: %+v, url %q", c, c.URL())
	}
}

func TestPublic(t *testing.T) {
	for ip, want := range map[string]bool{
		"203.0.113.7": true, "8.8.8.8": true, "2001:4860::8888": true,
		"192.168.1.1": false, "10.0.0.1": false, "172.16.5.4": false, "100.64.0.1": false, "100.127.255.254": false,
		"127.0.0.1": false, "169.254.1.1": false, "fd00::1": false, "fe80::1": false,
	} {
		if got := Public(net.ParseIP(ip)); got != want {
			t.Errorf("Public(%s) = %v, want %v", ip, got, want)
		}
	}
	var none *Mapping
	if none.URL() != "" {
		t.Error("a nil mapping has a URL")
	}
}

func TestTakesTheNextPortWhenAnotherMachineHasIt(t *testing.T) {
	shortTimes(t)
	router := &fakeGateway{kind: "UPnP", ip: net.IPv4(203, 0, 113, 7), taken: map[uint16]bool{56398: true, 56399: true}}
	m := &Mapper{Port: 56398, Find: func(context.Context) []Gateway { return []Gateway{router} }}
	run(t, m)
	waitFor(t, "a mapping", func() bool { return m.Current() != nil })
	if c := m.Current(); c.ExternalPort != 56400 || c.URL() != "https://203.0.113.7:56400" {
		t.Fatalf("mapping: %+v", c)
	}
	waitFor(t, "a renewal", func() bool { n, _ := router.counts(); return n >= 2 })
	if c := m.Current(); c == nil || c.ExternalPort != 56400 {
		t.Fatalf("after renewing: %+v", c)
	}
}
