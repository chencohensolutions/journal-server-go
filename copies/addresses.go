package copies

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync/atomic"
	"time"
)

// AddressBook is where a host may be beside the address it was added with: the addresses that worked before and
// those it told us, and the DHT for when none answer (docs/peers.md, "Finding the host"). The host is checked by its
// key on every connection, so an address is only a hint: one that isn't the host fails the TLS handshake.
type AddressBook interface {
	// Addresses to try, as host:port, the best first.
	Addresses() []string
	// Worked notes the address a connection was made to.
	Worked(hostport string)
	// Discover asks for addresses afresh (the DHT), when none of the known ones answer. An error, or nothing new,
	// leaves the host out of reach.
	Discover(ctx context.Context) ([]string, error)
}

// dialer dials a host by any of its addresses: the one in the request's URL is only the first guess.
type dialer struct {
	net  net.Dialer
	book atomic.Pointer[AddressBook]
}

// stagger is how long one address is given before the next is tried beside it, as browsers do ("happy eyeballs").
var stagger = 400 * time.Millisecond

func (d *dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	bp := d.book.Load()
	if bp == nil {
		return d.net.DialContext(ctx, network, addr)
	}
	book := *bp
	candidates := book.Addresses()
	if !slices.Contains(candidates, addr) {
		candidates = append(candidates, addr)
	}
	conn, err := d.race(ctx, network, candidates, book)
	if err == nil || ctx.Err() != nil {
		return conn, err
	}
	found, derr := book.Discover(ctx)
	var fresh []string
	for _, a := range found {
		if !slices.Contains(candidates, a) {
			fresh = append(fresh, a)
		}
	}
	if len(fresh) == 0 {
		return nil, errors.Join(err, derr)
	}
	return d.race(ctx, network, fresh, book)
}

// race dials each address in turn, stagger apart, and keeps the first connection made.
func (d *dialer) race(ctx context.Context, network string, addrs []string, book AddressBook) (net.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		conn net.Conn
		addr string
		err  error
	}
	results := make(chan result, len(addrs))
	started, failed := 0, 0
	var errs []error
	start := func() {
		a := addrs[started]
		started++
		go func() {
			c, err := d.net.DialContext(ctx, network, a)
			results <- result{c, a, err}
		}()
	}
	start()
	timer := time.NewTimer(stagger)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
		case r := <-results:
			if r.err == nil {
				book.Worked(r.addr)
				// Connections still being made are closed as they arrive.
				go func(left int) {
					for range left {
						if r := <-results; r.conn != nil {
							r.conn.Close()
						}
					}
				}(started - failed - 1)
				return r.conn, nil
			}
			failed++
			errs = append(errs, r.err)
			if failed == len(addrs) {
				return nil, errors.Join(errs...)
			}
			if failed < started {
				continue
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		// The timer fired, or every dial so far failed: start the next address now.
		if started < len(addrs) {
			start()
			timer.Reset(stagger) // since Go 1.23 a reset timer never delivers a stale tick
		}
	}
}
