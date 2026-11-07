package discovery

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/dht/v2/bep44"
	"github.com/anacrolix/dht/v2/exts/getput"
	"github.com/anacrolix/dht/v2/krpc"
	alog "github.com/anacrolix/log"
	"github.com/anacrolix/torrent/bencode"
)

// quiet keeps the DHT library's chatter (nodes that don't answer, refused puts) out of the server's log.
var quiet = alog.Default.FilterLevel(alog.Critical)

// Node is this process's place in the BitTorrent DHT, started at its first use. It only asks: it answers no other
// node's queries (passive, BEP 43's spirit), so it costs a few UDP packets a lookup and nothing between them.
type Node struct {
	// Where it joins the DHT; the BitTorrent routers (router.bittorrent.com and others) when nil.
	Bootstrap func() ([]dht.Addr, error)
	// Answer other nodes too (tests run a small DHT of their own on loopback).
	Active bool
	listen string // the UDP address to listen on; any port when empty

	mu     sync.Mutex
	server *dht.Server
}

// started is the DHT server, joined to the DHT the first time.
func (n *Node) started(ctx context.Context) (*dht.Server, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.server != nil {
		return n.server, nil
	}
	listen := n.listen
	if listen == "" {
		listen = ":0"
	}
	conn, err := net.ListenPacket("udp", listen)
	if err != nil {
		return nil, fmt.Errorf("the DHT needs a UDP socket: %w", err)
	}
	config := dht.NewDefaultServerConfig()
	config.Conn = conn
	config.Passive = !n.Active
	config.Logger = quiet
	if n.Bootstrap != nil {
		config.StartingNodes = n.Bootstrap
	}
	s, err := dht.NewServer(config)
	if err != nil {
		conn.Close()
		return nil, err
	}
	// Joining fills the routing table; lookups start from it, and from the starting nodes until it has some.
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Bootstrap()
	}()
	select {
	case <-done:
	case <-ctx.Done():
	case <-time.After(20 * time.Second):
	}
	n.server = s
	return s, nil
}

// Close leaves the DHT.
func (n *Node) Close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.server != nil {
		n.server.Close()
		n.server = nil
	}
}

// Publish keeps r in the DHT under key, sealed for those who know fingerprint. Nodes keep a record for about two
// hours, so it is published again well before (Publisher).
func (n *Node) Publish(ctx context.Context, key ed25519.PrivateKey, fingerprint string, r Record) error {
	r.V = 1
	sealed, err := seal(r, fingerprint)
	if err != nil {
		return err
	}
	s, err := n.started(ctx)
	if err != nil {
		return err
	}
	var k [32]byte
	copy(k[:], key.Public().(ed25519.PublicKey))
	target := bep44.MakeMutableTarget(k, salt)
	seq := r.At.Unix()
	stats, err := getput.Put(alog.ContextWithLogger(ctx, quiet), krpc.ID(target), s, salt, func(found int64) bep44.Put {
		put := bep44.Put{V: sealed, K: &k, Salt: salt, Seq: max(seq, found+1)}
		put.Sign(key)
		return put
	})
	if err != nil {
		return err
	}
	if stats != nil && stats.NumResponses == 0 {
		return errors.New("no DHT node answered")
	}
	return nil
}

// ErrNotFound is a lookup that found no record: the server hasn't published one in the last hours.
var ErrNotFound = errors.New("no record of the server in the DHT")

// Lookup is the record the server with DHT key and fingerprint published, the newest the DHT has.
func (n *Node) Lookup(ctx context.Context, key ed25519.PublicKey, fingerprint string) (Record, error) {
	if len(key) != ed25519.PublicKeySize {
		return Record{}, errors.New("no DHT key for the server")
	}
	s, err := n.started(ctx)
	if err != nil {
		return Record{}, err
	}
	var k [32]byte
	copy(k[:], key)
	got, _, err := getput.Get(alog.ContextWithLogger(ctx, quiet), bep44.MakeMutableTarget(k, salt), s, nil, salt)
	if err != nil {
		if ctx.Err() == nil {
			return Record{}, ErrNotFound
		}
		return Record{}, err
	}
	var sealed []byte
	if err := bencode.Unmarshal(got.V, &sealed); err != nil {
		return Record{}, fmt.Errorf("the DHT's record isn't one of ours: %w", err)
	}
	return open(sealed, fingerprint)
}
