package discovery

import (
	"net"
	"net/netip"
	"slices"
	"strings"
)

// Own is where this server can be reached, the most likely first: publicURL (config.json's public_url) when set,
// mapped (the router's outside address and port, when the router forwards one to this machine: package portmap),
// the public IPv4 address (STUN, or seen by a peer) at port, then each global IPv6 address, then the LAN addresses
// (for phones at home and servers on the same network). A public IPv4 address works only when the router forwards
// port to this machine; an IPv6 one needs only the firewall rule.
func Own(publicURL, mapped, port string, publicIPv4 net.IP) []string {
	var out []string
	add := func(url string) {
		if url != "" && !slices.Contains(out, url) {
			out = append(out, url)
		}
	}
	add(strings.TrimSuffix(publicURL, "/"))
	add(mapped)
	if publicIPv4 != nil {
		add(URLOf(publicIPv4, port))
	}
	var lan []string
	for _, ip := range interfaceAddresses() {
		switch {
		case ip.Is6() && !ip.Is4In6() && ip.IsGlobalUnicast() && !ip.IsPrivate():
			add(URLOf(net.IP(ip.AsSlice()), port))
		case ip.Is4() && ip.IsPrivate():
			lan = append(lan, URLOf(net.IP(ip.AsSlice()), port))
		case ip.Is4() && ip.IsGlobalUnicast() && !ip.IsPrivate() && !isShared(ip):
			add(URLOf(net.IP(ip.AsSlice()), port)) // a public address on the machine itself
		}
	}
	for _, url := range lan {
		add(url)
	}
	return out
}

// URLOf is the server's address at ip.
func URLOf(ip net.IP, port string) string {
	return "https://" + net.JoinHostPort(ip.String(), port)
}

// isShared is carrier-grade NAT space (100.64.0.0/10): not reachable from outside.
func isShared(ip netip.Addr) bool {
	return netip.MustParsePrefix("100.64.0.0/10").Contains(ip)
}

func interfaceAddresses() []netip.Addr {
	var out []netip.Addr
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			prefix, err := netip.ParsePrefix(a.String())
			if err != nil {
				continue
			}
			out = append(out, prefix.Addr().Unmap())
		}
	}
	return out
}

// HostPorts are the dialable host:port of urls ("https://host:port").
func HostPorts(urls []string) []string {
	var out []string
	for _, u := range urls {
		rest, ok := strings.CutPrefix(u, "https://")
		if !ok {
			continue
		}
		host, _, _ := strings.Cut(rest, "/")
		if _, _, err := net.SplitHostPort(host); err == nil && !slices.Contains(out, host) {
			out = append(out, host)
		}
	}
	return out
}

func netipOf(ip net.IP) netip.Addr {
	a, _ := netip.AddrFromSlice(ip)
	return a.Unmap()
}
