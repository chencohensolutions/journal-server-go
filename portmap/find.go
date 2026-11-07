package portmap

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/huin/goupnp/dcps/internetgateway2"
	"github.com/jackpal/gateway"
	natpmp "github.com/jackpal/go-nat-pmp"
)

// Find asks the network for routers that map ports: UPnP IGD (SSDP) and NAT-PMP on the default gateway, at once.
// UPnP comes first: more home routers speak it.
func Find(ctx context.Context) []Gateway {
	var mu sync.Mutex
	var upnp, pmp []Gateway
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		found := findUPnP(ctx)
		mu.Lock()
		upnp = found
		mu.Unlock()
	}()
	go func() {
		defer wg.Done()
		if g := findNATPMP(ctx); g != nil {
			mu.Lock()
			pmp = []Gateway{g}
			mu.Unlock()
		}
	}()
	wg.Wait()
	return append(upnp, pmp...)
}

// upnpClient is what the IGD services this package uses have in common.
type upnpClient interface {
	AddPortMappingCtx(ctx context.Context, remoteHost string, externalPort uint16, protocol string, internalPort uint16,
		internalClient string, enabled bool, description string, leaseDuration uint32) error
	DeletePortMappingCtx(ctx context.Context, remoteHost string, externalPort uint16, protocol string) error
	GetExternalIPAddressCtx(ctx context.Context) (string, error)
	LocalAddr() net.IP
}

func findUPnP(ctx context.Context) []Gateway {
	var out []Gateway
	if cs, _, err := internetgateway2.NewWANIPConnection2ClientsCtx(ctx); err == nil {
		for _, c := range cs {
			out = append(out, &upnpGateway{c})
		}
	}
	if cs, _, err := internetgateway2.NewWANIPConnection1ClientsCtx(ctx); err == nil {
		for _, c := range cs {
			out = append(out, &upnpGateway{c})
		}
	}
	if cs, _, err := internetgateway2.NewWANPPPConnection1ClientsCtx(ctx); err == nil {
		for _, c := range cs {
			out = append(out, &upnpGateway{c})
		}
	}
	return out
}

type upnpGateway struct{ c upnpClient }

func (g *upnpGateway) Kind() string { return "UPnP" }

func (g *upnpGateway) Map(ctx context.Context, internal, external uint16, lease time.Duration, description string) (uint16, error) {
	local := g.c.LocalAddr()
	if local == nil {
		return 0, errors.New("this machine's address toward the router is unknown")
	}
	err := g.c.AddPortMappingCtx(ctx, "", external, "TCP", internal, local.String(), true, description, uint32(lease.Seconds()))
	if err != nil {
		// Some routers only take permanent mappings (error 725, OnlyPermanentLeasesSupported); it is removed on stop.
		if err2 := g.c.AddPortMappingCtx(ctx, "", external, "TCP", internal, local.String(), true, description, 0); err2 == nil {
			return external, nil
		}
		return 0, err
	}
	return external, nil
}

func (g *upnpGateway) Unmap(ctx context.Context, _, external uint16) error {
	return g.c.DeletePortMappingCtx(ctx, "", external, "TCP")
}

func (g *upnpGateway) ExternalIP(ctx context.Context) (net.IP, error) {
	s, err := g.c.GetExternalIPAddressCtx(ctx)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return nil, errors.New("the router's outside address isn't an IP: " + s)
	}
	return ip, nil
}

func findNATPMP(ctx context.Context) Gateway {
	ip, err := gateway.DiscoverGateway()
	if err != nil || ip.To4() == nil {
		return nil
	}
	g := &pmpGateway{natpmp.NewClientWithTimeout(ip, 2*time.Second)}
	if _, err := g.ExternalIP(ctx); err != nil {
		return nil // the gateway doesn't speak NAT-PMP
	}
	return g
}

type pmpGateway struct{ c *natpmp.Client }

func (g *pmpGateway) Kind() string { return "NAT-PMP" }

func (g *pmpGateway) Map(_ context.Context, internal, external uint16, lease time.Duration, _ string) (uint16, error) {
	r, err := g.c.AddPortMapping("tcp", int(internal), int(external), int(lease.Seconds()))
	if err != nil {
		return 0, err
	}
	return r.MappedExternalPort, nil
}

func (g *pmpGateway) Unmap(_ context.Context, internal, _ uint16) error {
	// NAT-PMP removes a mapping asked for again with a lifetime of 0; it is named by its internal port (RFC 6886).
	_, err := g.c.AddPortMapping("tcp", int(internal), 0, 0)
	return err
}

func (g *pmpGateway) ExternalIP(context.Context) (net.IP, error) {
	r, err := g.c.GetExternalAddress()
	if err != nil {
		return nil, err
	}
	return net.IP(r.ExternalIPAddress[:]).To4(), nil
}
