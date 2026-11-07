package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/copies"
	"github.com/journal/server-go/discovery"
	"github.com/journal/server-go/graph/model"
	"github.com/journal/server-go/peers"
	"github.com/journal/server-go/portmap"
)

// Finding servers whose address changes (docs/peers.md, "Finding the host"). This server publishes where it is in
// the BitTorrent DHT and tells its peers when they ask; each peer it keeps copies on has an address book (peerBook)
// of the addresses that worked and those the peer told, and looks the peer up in the DHT when none answer.

// serverDiscovery is this server's side: its id, its DHT key and the publisher of its addresses.
type serverDiscovery struct {
	fingerprint string
	key         ed25519.PrivateKey // nil with "discovery": false
	publisher   *discovery.Publisher
}

// dhtNode is this process's place in the DHT, shared by the publisher and every lookup; it joins at first use.
var dhtNode = &discovery.Node{}

// openDiscovery reads this server's key for discovery. Without "discovery": false the publisher publishes in the DHT
// once run.
func openDiscovery(config auth.AppConfig) (*serverDiscovery, error) {
	pair, err := tls.LoadX509KeyPair(config.TLSCert, config.TLSKey)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	d := &serverDiscovery{fingerprint: peers.Fingerprint(leaf)}
	d.publisher = &discovery.Publisher{Fingerprint: d.fingerprint, PublicURL: config.PublicURL, Port: config.Port,
		STUN: discovery.STUNServers}
	if config.DiscoveryOn() {
		if d.key, err = discovery.KeyFor(pair.PrivateKey); err != nil {
			return nil, err
		}
		d.publisher.Node, d.publisher.Key = dhtNode, d.key
	}
	return d, nil
}

// mapPort asks the router to forward the server's port to this machine while it runs (package portmap), unless
// config.json sets "port_mapping": false, and gives the outside address to d's publisher. done is closed once the
// mapping is removed after running ends (at once when there is none).
func mapPort(running context.Context, config auth.AppConfig, d *serverDiscovery, done chan<- struct{}) {
	port, err := strconv.ParseUint(config.Port, 10, 16)
	if !config.PortMappingOn() || err != nil {
		close(done)
		return
	}
	m := &portmap.Mapper{Port: uint16(port), Description: "Journal server"}
	d.publisher.Mapped = func() string { return m.Current().URL() }
	go func() { m.Run(running); close(done) }()
}

// printRouters lists the routers that map ports and their outside addresses, for `journal-server discovery`. It only
// asks: mapping the port from here would take over the running server's mapping and remove it on exit.
func printRouters(ctx context.Context, config auth.AppConfig) {
	if !config.PortMappingOn() {
		fmt.Printf("Port mapping is off in config.json (\"port_mapping\": false)\n\n")
		return
	}
	ask, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	gws := portmap.Find(ask)
	if len(gws) == 0 {
		fmt.Printf("No router here maps ports (UPnP and NAT-PMP off, or none answers): forward port %s by hand, or use IPv6\n\n", config.Port)
		return
	}
	fmt.Printf("Routers that map ports (the running server asks them to forward port %s):\n", config.Port)
	for _, g := range gws {
		ip, err := g.ExternalIP(ask)
		switch {
		case err != nil:
			fmt.Printf("  %s: outside address unknown: %v\n", g.Kind(), err)
		case !portmap.Public(ip):
			fmt.Printf("  %s: outside address %s isn't public (carrier-grade NAT, or another router in front): the internet can't reach this server through it\n", g.Kind(), ip)
		default:
			fmt.Printf("  %s: outside address %s\n", g.Kind(), ip)
		}
	}
	fmt.Println()
}

// thisServer is the running server's discovery, for phones (serverInfo.discovery).
var thisServer atomic.Pointer[serverDiscovery]

// forPhones is serverInfo.discovery: who this server is and where, or nil before it is known.
func forPhones() *model.ServerDiscovery {
	d := thisServer.Load()
	if d == nil {
		return nil
	}
	found := d.forPeers()
	out := &model.ServerDiscovery{Fingerprint: d.fingerprint, ID: discovery.FormatID(d.fingerprint),
		Addresses: found.Addresses}
	if found.Key != "" {
		out.Key = &found.Key
	}
	return out
}

// forPeers is what GET /peer/addresses and an enrolling peer get: where this server is, and its DHT key.
func (d *serverDiscovery) forPeers() peers.Discovery {
	out := peers.Discovery{Addresses: d.publisher.Current()}
	if d.key != nil {
		out.Key = hex.EncodeToString(d.key.Public().(ed25519.PublicKey))
	}
	return out
}

// peerInfoMu keeps one rewrite of a peer.json at a time in this process.
var peerInfoMu sync.Mutex

// updatePeerInfo rewrites the peer.json of the peer with that key (peerInfo.key) with change, written to a temp
// file and renamed.
func updatePeerInfo(config auth.AppConfig, key string, change func(*peerInfo)) error {
	peerInfoMu.Lock()
	defer peerInfoMu.Unlock()
	path := filepath.Join(peerDir(config, key), "peer.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var info peerInfo
	if err := json.Unmarshal(data, &info); err != nil || info.V != 1 {
		return fmt.Errorf("%s isn't readable: %v", path, err)
	}
	change(&info)
	data, _ = json.MarshalIndent(info, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// maxAddresses is the most a peer's address book keeps.
const maxAddresses = 16

// peerBook is the address book of one peer this server keeps copies on (copies.AddressBook), kept in its peer.json.
type peerBook struct {
	config      auth.AppConfig
	name        string // for the log
	key         string // its folder in peers_dir (peerInfo.key)
	fingerprint string
	// A book for enrolling, before the peer is kept: nothing is written.
	unsaved bool

	mu         sync.Mutex
	addrs      []string // host:port, the one that last worked first
	dhtKey     string   // the peer's DHT key, hex
	lookedUpAt time.Time
}

var (
	booksMu sync.Mutex
	books   = map[string]*peerBook{} // by peers_dir and name: one book per peer in the process
)

// lookupPeer finds a peer's record in the DHT (tests replace it).
var lookupPeer = func(ctx context.Context, key ed25519.PublicKey, fingerprint string) (discovery.Record, error) {
	return dhtNode.Lookup(ctx, key, fingerprint)
}

// lookupEvery is how often a peer out of reach is looked up in the DHT, at most.
var lookupEvery = 2 * time.Minute

// bookFor is the address book of the peer info describes.
func bookFor(config auth.AppConfig, info peerInfo) *peerBook {
	booksMu.Lock()
	defer booksMu.Unlock()
	id := config.PeersDir + "\x00" + info.key()
	if b, ok := books[id]; ok {
		return b
	}
	b := &peerBook{config: config, name: info.Name, key: info.key(), fingerprint: info.Fingerprint,
		dhtKey: info.DiscoveryKey, addrs: slices.Clone(info.Addresses)}
	books[id] = b
	return b
}

// forgetBook drops the book of a peer removed, so one added again by the same name starts afresh.
func forgetBook(config auth.AppConfig, info peerInfo) {
	booksMu.Lock()
	defer booksMu.Unlock()
	delete(books, config.PeersDir+"\x00"+info.key())
}

func (b *peerBook) Addresses() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.addrs)
}

func (b *peerBook) Worked(hostport string) {
	b.mu.Lock()
	if len(b.addrs) > 0 && b.addrs[0] == hostport {
		b.mu.Unlock()
		return
	}
	moved := len(b.addrs) > 0 // not the first address the peer was ever reached at
	b.addrs = withFirst(b.addrs, hostport)
	addrs := slices.Clone(b.addrs)
	b.mu.Unlock()
	if moved && !b.unsaved {
		log.Printf("Peer %s: reached at %s", b.name, hostport)
	}
	if b.unsaved {
		return
	}
	if err := updatePeerInfo(b.config, b.key, func(i *peerInfo) { i.Addresses = addrs }); err != nil {
		log.Printf("Peer %s: keeping its address: %v", b.name, err)
	}
}

func (b *peerBook) Discover(ctx context.Context) ([]string, error) {
	b.mu.Lock()
	keyHex, recent := b.dhtKey, time.Since(b.lookedUpAt) < lookupEvery
	if !recent {
		b.lookedUpAt = time.Now()
	}
	b.mu.Unlock()
	switch {
	case !b.config.DiscoveryOn():
		return nil, errors.New(`not looked up in the DHT ("discovery": false)`)
	case keyHex == "":
		return nil, errors.New("not looked up in the DHT: the peer hasn't told its DHT key yet (it does once reached)")
	case recent:
		return nil, errors.New("looked up in the DHT a moment ago")
	}
	key, err := discovery.ParseKey(keyHex)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	record, err := lookupPeer(ctx, key, b.fingerprint)
	if err != nil {
		return nil, fmt.Errorf("looking the peer up in the DHT: %w", err)
	}
	found := discovery.HostPorts(record.Addresses)
	log.Printf("Peer %s: the DHT has it at %s (published %s)", b.name, strings.Join(found, ", "),
		record.At.Local().Format("2006-01-02 15:04"))
	b.learn(found, "")
	return found, nil
}

// learn adds addresses the peer told or the DHT gave after the one that last worked, and its DHT key when given.
func (b *peerBook) learn(found []string, key string) {
	b.mu.Lock()
	merged := slices.Clone(b.addrs[:min(1, len(b.addrs))])
	for _, a := range slices.Concat(found, b.addrs) {
		if !slices.Contains(merged, a) {
			merged = append(merged, a)
		}
	}
	merged = merged[:min(len(merged), maxAddresses)]
	changed := !slices.Equal(merged, b.addrs) || (key != "" && key != b.dhtKey)
	b.addrs = merged
	if key != "" {
		b.dhtKey = key
	}
	key = b.dhtKey
	b.mu.Unlock()
	if !changed || b.unsaved {
		return
	}
	if err := updatePeerInfo(b.config, b.key, func(i *peerInfo) { i.Addresses, i.DiscoveryKey = merged, key }); err != nil {
		log.Printf("Peer %s: keeping its addresses: %v", b.name, err)
	}
}

func withFirst(list []string, first string) []string {
	out := []string{first}
	for _, a := range list {
		if a != first {
			out = append(out, a)
		}
	}
	return out[:min(len(out), maxAddresses)]
}

// peerFor is a client of the peer info describes, as user (none for the peer API's own routes), reaching it by
// its address book.
func peerFor(config auth.AppConfig, info peerInfo, user string, cert tls.Certificate) *copies.Peer {
	p := copies.NewPeer(info.Name, info.URL, user, cert, info.Fingerprint)
	p.Key = info.key()
	p.SetAddressBook(bookFor(config, info))
	return p
}

// askPeerAddresses asks each peer this server keeps copies on where it is now (GET /peer/addresses) and keeps the
// answer in its book; what the peers saw this server's requests come from goes to d's publisher.
func askPeerAddresses(ctx context.Context, config auth.AppConfig, d *serverDiscovery) {
	for _, key := range peerKeys(config) {
		info, cert, err := loadPeer(config, key)
		if err != nil {
			continue
		}
		book := bookFor(config, info)
		client := copies.PeerClient(cert, info.Fingerprint, book)
		ask, cancel := context.WithTimeout(ctx, 2*time.Minute)
		answer, err := getPeerAddresses(ask, client, info.URL)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("Peer %s: asking where it is: %v", info.Name, err)
			}
			continue
		}
		book.learn(discovery.HostPorts(answer.Addresses), answer.Key)
		if d != nil {
			d.publisher.Saw(net.ParseIP(answer.Seen))
		}
	}
}

func getPeerAddresses(ctx context.Context, client *http.Client, url string) (peers.Discovery, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/peer/addresses", nil)
	if err != nil {
		return peers.Discovery{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return peers.Discovery{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return peers.Discovery{}, errors.New("its server is older and doesn't tell (GET /peer/addresses)")
	}
	if resp.StatusCode != http.StatusOK {
		return peers.Discovery{}, fmt.Errorf("it answered %s", resp.Status)
	}
	var answer peers.Discovery
	err = json.NewDecoder(resp.Body).Decode(&answer)
	return answer, err
}

// askPeerAddressesEvery asks the peers where they are at start and every half hour until ctx ends.
func askPeerAddressesEvery(ctx context.Context, config auth.AppConfig, d *serverDiscovery) {
	for {
		askPeerAddresses(ctx, config, d)
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Minute):
		}
	}
}

// idCommand: journal-server id. This server's id, as others name it.
func idCommand(config auth.AppConfig) error {
	d, err := openDiscovery(config)
	if err != nil {
		return err
	}
	fmt.Printf("Server id:   %s\n", discovery.FormatID(d.fingerprint))
	fmt.Printf("Fingerprint: %s\n", d.fingerprint)
	if d.key != nil {
		fmt.Printf("DHT key:     %s\n", hex.EncodeToString(d.key.Public().(ed25519.PublicKey)))
	} else {
		fmt.Printf("DHT:         off (\"discovery\": false)\n")
	}
	return nil
}

// discoveryCommand: journal-server discovery. Works out where this server is, publishes it in the DHT, and looks
// each peer up there, to check discovery works from this network. The running server does the same by itself.
func discoveryCommand(config auth.AppConfig) error {
	d, err := openDiscovery(config)
	if err != nil {
		return err
	}
	if d.key == nil {
		return errors.New(`discovery is off in config.json ("discovery": false)`)
	}
	defer dhtNode.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	fmt.Printf("Server id %s\n\n", discovery.FormatID(d.fingerprint))
	printRouters(ctx, config)
	addresses := d.publisher.Addresses(ctx)
	fmt.Printf("This server's addresses:\n")
	for _, a := range addresses {
		fmt.Printf("  %s\n", a)
	}
	fmt.Printf("\nPublishing them in the DHT (joining it takes a few seconds)...\n")
	if err := dhtNode.Publish(ctx, d.key, d.fingerprint, discovery.Record{Addresses: addresses, At: time.Now().UTC()}); err != nil {
		fmt.Printf("  failed: %v\n", err)
	} else {
		fmt.Printf("  done\n")
	}
	if record, err := dhtNode.Lookup(ctx, d.key.Public().(ed25519.PublicKey), d.fingerprint); err != nil {
		fmt.Printf("  looking it up again: %v\n", err)
	} else {
		fmt.Printf("  found again: %s\n", strings.Join(record.Addresses, ", "))
	}
	for _, pk := range peerKeys(config) {
		info, _, err := loadPeer(config, pk)
		if err != nil {
			continue
		}
		fmt.Printf("\nPeer %s (%s), last reached at %s:\n", info.Name, discovery.FormatID(info.Fingerprint),
			strings.Join(info.Addresses[:min(1, len(info.Addresses))], ""))
		key, err := discovery.ParseKey(info.DiscoveryKey)
		if err != nil {
			fmt.Printf("  no DHT key yet: the server learns it once it reaches the peer\n")
			continue
		}
		if record, err := dhtNode.Lookup(ctx, key, info.Fingerprint); err != nil {
			fmt.Printf("  %v\n", err)
		} else {
			fmt.Printf("  in the DHT, published %s: %s\n", record.At.Local().Format("2006-01-02 15:04"),
				strings.Join(record.Addresses, ", "))
		}
	}
	return nil
}
