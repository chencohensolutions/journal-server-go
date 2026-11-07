package discovery

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"time"
)

// STUN servers that tell a client the address its packets come from (RFC 5389 binding requests): how a server
// behind a home router learns its public IPv4 address. Each is asked in turn until one answers.
var STUNServers = []string{"stun.l.google.com:19302", "stun.cloudflare.com:3478", "stun.nextcloud.com:3478"}

const stunMagic = 0x2112A442

// PublicIPv4 is this machine's address as the internet sees it, from the first STUN server that answers.
func PublicIPv4(ctx context.Context, servers []string) (net.IP, error) {
	err := errors.New("no STUN server")
	for _, server := range servers {
		var ip net.IP
		if ip, err = askSTUN(ctx, server); err == nil {
			return ip, nil
		}
	}
	return nil, err
}

func askSTUN(ctx context.Context, server string) (net.IP, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp4", server)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	request := make([]byte, 20)
	binary.BigEndian.PutUint16(request[0:], 0x0001) // binding request, no attributes
	binary.BigEndian.PutUint32(request[4:], stunMagic)
	rand.Read(request[8:20])
	answer := make([]byte, 1500)
	for try := 0; try < 3; try++ {
		if _, err := conn.Write(request); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(time.Second)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		conn.SetReadDeadline(deadline)
		n, err := conn.Read(answer)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		if ip := mappedAddress(answer[:n], request[8:20]); ip != nil {
			return ip, nil
		}
	}
	return nil, errors.New(server + " didn't answer")
}

// mappedAddress is the IPv4 address in a binding success answer to transaction id, or nil.
func mappedAddress(m, id []byte) net.IP {
	if len(m) < 20 || binary.BigEndian.Uint16(m[0:]) != 0x0101 || binary.BigEndian.Uint32(m[4:]) != stunMagic ||
		string(m[8:20]) != string(id) {
		return nil
	}
	attrs := m[20:min(len(m), 20+int(binary.BigEndian.Uint16(m[2:])))]
	for len(attrs) >= 4 {
		kind, size := binary.BigEndian.Uint16(attrs[0:]), int(binary.BigEndian.Uint16(attrs[2:]))
		if len(attrs) < 4+size {
			return nil
		}
		value := attrs[4 : 4+size]
		// XOR-MAPPED-ADDRESS, or the old MAPPED-ADDRESS: reserved byte, family 1 (IPv4), port, address.
		if (kind == 0x0020 || kind == 0x0001) && size >= 8 && value[1] == 1 {
			ip := net.IPv4(value[4], value[5], value[6], value[7]).To4()
			if kind == 0x0020 {
				var magic [4]byte
				binary.BigEndian.PutUint32(magic[:], stunMagic)
				for i := range ip {
					ip[i] ^= magic[i]
				}
			}
			return ip
		}
		attrs = attrs[4+(size+3)/4*4:]
	}
	return nil
}
