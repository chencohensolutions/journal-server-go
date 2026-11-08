package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/copies"
	"github.com/journal/server-go/discovery"
	"github.com/journal/server-go/enroll"
	"github.com/journal/server-go/peers"
)

// A peer this server keeps copies on is a folder in peers_dir (certs/peers/<name>/): key.pem and cert.pem (the
// client certificate the host signed) and peer.json. See docs/peers.md. A peer a user added from their phone (a swap,
// swap.go) is that user's: certs/peers/<user>/<name>/, used only for that user's copy, which its peer.json describes.

type peerInfo struct {
	V    int    `json:"v"`
	Name string `json:"name"` // as peer add named it here
	URL  string `json:"url"`
	// The host server's key (peers.Fingerprint), from the invite: the only server this peer's copies go to.
	Fingerprint string `json:"fingerprint"`
	// The owner accepted that the host's admin can read plain copies there.
	Trusted bool `json:"trusted"`
	// The name the host knows this server by (its invite's), and the quota it gave.
	HostName  string    `json:"host_name"`
	Quota     int64     `json:"quota"`
	AddedAt   time.Time `json:"added_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// Where the host was reached and where it said it is, host:port, the one that last worked first; URL's is
	// tried too. Kept up by the server (discover.go).
	Addresses []string `json:"addresses,omitempty"`
	// The host's key in the DHT (hex), which it publishes its addresses under; from the host when it was added or
	// first asked (GET /peer/addresses).
	DiscoveryKey string `json:"discovery_key,omitempty"`
	// The user whose peer it is, added from their phone (joinSwap); "" for one added with peer add, the server's.
	User string `json:"user,omitempty"`
	// The host's user whose invite it was (a swap), "" for the host's own (peer invite).
	HostUser string `json:"host_user,omitempty"`
	// That user's copy there; config.json's copies name only the server's peers.
	Copy *swapCopy `json:"copy,omitempty"`
}

// swapCopy is a user's copy on their own peer.
type swapCopy struct {
	Encrypt bool   `json:"encrypt"`
	At      string `json:"at"`
}

// key is the peer's folder in peers_dir: its name, or <user>/<name> for a user's own.
func (i peerInfo) key() string { return peerKey(i.User, i.Name) }

func peerKey(user, name string) string {
	if user != "" {
		return filepath.Join(user, name)
	}
	return name
}

// peerDir is the folder of the peer with that key (peerInfo.key) in peers_dir.
func peerDir(config auth.AppConfig, key string) string { return filepath.Join(config.PeersDir, key) }

// peerKeys are the peers added here: the server's (peer add) and each user's (swaps), as their keys.
func peerKeys(config auth.AppConfig) []string {
	var keys []string
	isPeer := func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, "peer.json"))
		return err == nil
	}
	entries, _ := os.ReadDir(config.PeersDir)
	for _, e := range entries {
		if e.IsDir() && !strings.Contains(e.Name(), ".removed-") && isPeer(peerDir(config, e.Name())) {
			keys = append(keys, e.Name())
		}
	}
	for _, user := range config.Users {
		if slices.Contains(keys, user.Username) {
			continue // a peer added before the user was (peer add refuses users' names)
		}
		mine, _ := os.ReadDir(filepath.Join(config.PeersDir, user.Username))
		for _, e := range mine {
			key := peerKey(user.Username, e.Name())
			if e.IsDir() && !strings.Contains(e.Name(), ".removed-") && isPeer(peerDir(config, key)) {
				keys = append(keys, key)
			}
		}
	}
	return keys
}

// loadPeer reads the peer with that key (peerInfo.key): one added with peer add, or a user's.
func loadPeer(config auth.AppConfig, key string) (peerInfo, tls.Certificate, error) {
	dir := peerDir(config, key)
	data, err := os.ReadFile(filepath.Join(dir, "peer.json"))
	if errors.Is(err, os.ErrNotExist) {
		return peerInfo{}, tls.Certificate{}, fmt.Errorf("no peer %q: add it with journal-server peer add", key)
	}
	if err != nil {
		return peerInfo{}, tls.Certificate{}, err
	}
	var info peerInfo
	if err := json.Unmarshal(data, &info); err != nil || info.V != 1 {
		return peerInfo{}, tls.Certificate{}, fmt.Errorf("%s isn't readable: %v", filepath.Join(dir, "peer.json"), err)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		return peerInfo{}, tls.Certificate{}, fmt.Errorf("peer %s: %w", key, err)
	}
	return info, cert, nil
}

// peerTarget is the copy of user's data folder on the peer cc names: encrypted with the user's copy key when cc says
// so, else plain, which goes only to a trusted peer.
func peerTarget(config auth.AppConfig, user string, cc auth.CopyConfig) (copies.Target, error) {
	info, cert, err := loadPeer(config, cc.Peer)
	if err != nil {
		return nil, err
	}
	if !info.Trusted && !cc.Encrypt {
		return nil, fmt.Errorf("peer %s isn't trusted: set encrypt on the copy (journal-server copy-key new %s), or add the peer again with --trusted to keep a plain copy there", cc.Peer, user)
	}
	p := peerFor(config, info, user, cert)
	p.BytesPerSecond = int64(cc.BandwidthKbps) * 1000 / 8
	if !cc.Encrypt {
		return p, nil
	}
	key, err := loadCopyKey(config, user)
	if err != nil {
		return nil, err
	}
	return copies.OpenEncrypted(p, key, filepath.Join(peerDir(config, cc.Peer), "copies", user))
}

// runPeerCommand runs peer invite | add | revoke | remove, and peers.
func runPeerCommand(config auth.AppConfig, command string, args []string) error {
	if command == "peers" && len(args) == 0 {
		return peersCommand(config)
	}
	if len(args) == 0 {
		return errors.New("peer needs invite, add, revoke or remove")
	}
	switch sub, rest := args[0], args[1:]; {
	case sub == "invite":
		return peerInviteCommand(config, rest)
	case sub == "add" && len(rest) >= 3:
		return peerAddCommand(config, rest)
	case sub == "revoke" && len(rest) == 1:
		return peerRevokeCommand(config, rest[0])
	case sub == "remove" && len(rest) == 1:
		return peerRemoveCommand(config, rest[0])
	}
	fmt.Fprint(os.Stderr, usage)
	return fmt.Errorf("unknown peer command or wrong arguments: %s", strings.Join(args, " "))
}

// peerInviteCommand: peer invite --name <name> --quota <size>.
func peerInviteCommand(config auth.AppConfig, args []string) error {
	var name, quotaText string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--name" && i+1 < len(args):
			name, i = args[i+1], i+1
		case args[i] == "--quota" && i+1 < len(args):
			quotaText, i = args[i+1], i+1
		default:
			return fmt.Errorf("peer invite --name <name> --quota <size, like 2TB>, not %q", args[i])
		}
	}
	quota, err := parseSize(quotaText)
	if name == "" || err != nil {
		return fmt.Errorf("peer invite --name <name> --quota <size, like 2TB>: %v", err)
	}
	fingerprint, err := serverFingerprint(config)
	if err != nil {
		return err
	}
	var data struct {
		CreatePeerInvite struct {
			Code      string    `json:"code"`
			ExpiresAt time.Time `json:"expiresAt"`
		} `json:"createPeerInvite"`
	}
	err = adminQuery(config, `mutation($name: String!, $quota: Float!) { createPeerInvite(name: $name, quota: $quota) { code expiresAt } }`,
		map[string]any{"name": name, "quota": float64(quota)}, &data)
	if err != nil {
		return err
	}
	invite := data.CreatePeerInvite
	address := config.PublicURL
	if address == "" {
		// No host name: this server's public address now. Once added, the peer follows it when it changes.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		ip, err := discovery.PublicIPv4(ctx, discovery.STUNServers)
		cancel()
		if err == nil {
			address = discovery.URLOf(ip, config.Port)
		} else {
			address = "https://<this server's address>:" + config.Port
		}
	}
	fmt.Printf("Invite for peer %q, up to %s\n\n", name, copies.Size(quota))
	fmt.Printf("  On the other server, run:\n\n    journal-server peer add <a name for this server> %s %s.%s [--trusted]\n\n",
		address, invite.Code, fingerprint)
	fmt.Printf("  Expires %s (in %d min), single use. --trusted lets that server keep plain copies here, which\n",
		invite.ExpiresAt.Local().Format("15:04"), int(time.Until(invite.ExpiresAt).Round(time.Minute).Minutes()))
	fmt.Printf("  this server's admin can read.\n")
	return nil
}

// serverFingerprint is this server's key, as a peer's invite names it.
func serverFingerprint(config auth.AppConfig) (string, error) {
	data, err := os.ReadFile(config.TLSCert)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return "", fmt.Errorf("%s: no certificate", config.TLSCert)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	return peers.Fingerprint(cert), nil
}

// peerAddCommand: peer add <name> <url> <code>.<fingerprint> [--trusted]. It enrolls this server with the host as a
// device enrolls, and keeps what it got in peers_dir/<name>/. The server needn't be running; it uses the peer once
// a copy in config.json names it and it is started again.
func peerAddCommand(config auth.AppConfig, args []string) error {
	name, address, invite := args[0], strings.TrimSuffix(args[1], "/"), args[2]
	trusted := false
	for _, arg := range args[3:] {
		if arg != "--trusted" {
			return fmt.Errorf("peer add <name> <url> <code> [--trusted], not %q", arg)
		}
		trusted = true
	}
	if !peers.ValidName(name) {
		return fmt.Errorf("%q can't name a peer: letters, digits and dashes", name)
	}
	if config.FindUser(name) != nil {
		return fmt.Errorf("%q is a user's name, which names the folder of their own peers: choose another", name)
	}
	if !strings.HasPrefix(address, "https://") {
		return fmt.Errorf("the host's address starts with https://, not %q", address)
	}
	code, id, ok := strings.Cut(invite, ".")
	fingerprint, err := discovery.ParseID(id)
	if !ok || err != nil {
		return fmt.Errorf("the code is the whole of what peer invite printed: the code, a dot and the server's fingerprint or id (%v)", err)
	}
	dir := peerDir(config, name)
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("peer %s is added already (%s); peer remove it first", name, dir)
	}
	host, _ := strings.CutPrefix(address, "https://")
	info, enrolled, err := enrollPeer(config, peerEnrollment{Name: name, Addresses: []string{host}, Code: code,
		Fingerprint: fingerprint, Trusted: trusted})
	if err != nil {
		return err
	}
	fmt.Printf("Peer %s added: %s knows this server as %q and keeps up to %s for it.\n\n", name, address,
		enrolled.Name, copies.Size(enrolled.Quota))
	if trusted {
		fmt.Printf("Marked trusted: plain copies may go there, and %s's admin can read them.\n\n", name)
	} else {
		fmt.Printf("Not trusted: only encrypted copies may go there, which aren't built yet.\n\n")
	}
	fmt.Printf("To keep a user's copy there, add to the user's \"copies\" in config.json and restart the server:\n\n")
	fmt.Printf("    { \"peer\": %q, \"at\": \"02:00\" }\n\n", name)
	fmt.Printf("Keep a backup of %s: without it, restoring from %s needs a new invite.\n", dir, name)
	_ = info
	return nil
}

// peerEnrollment is what enrolling with a host takes: an invite (its code and the host's fingerprint), where the
// host may be, and what this server keeps about it.
type peerEnrollment struct {
	Name string // the peer's name here
	User string // the user whose peer it is (a swap), or "" for the server's
	// host:port, the most likely first; and the host's DHT key (hex), to look it up when none answers.
	Addresses    []string
	DiscoveryKey string
	Code         string
	Fingerprint  string
	Trusted      bool
	Copy         *swapCopy
	// The swap offered back to the host's user (one scan, both ways).
	Offer *peers.Offer
}

// errHostRefused is a host that answered and refused the enrollment; errHostUnreachable one that couldn't be reached.
var (
	errHostRefused     = errors.New("the host refused")
	errHostUnreachable = errors.New("the host couldn't be reached")
)

// enrollPeer enrolls this server with the host as a device enrolls, and keeps what it got in its folder in
// peers_dir. The server needn't be running.
func enrollPeer(config auth.AppConfig, e peerEnrollment) (peerInfo, peers.Enrolled, error) {
	info := peerInfo{V: 1, Name: e.Name, User: e.User, Fingerprint: e.Fingerprint, Trusted: e.Trusted, Copy: e.Copy,
		DiscoveryKey: e.DiscoveryKey}
	for _, a := range e.Addresses {
		if a != "" && !slices.Contains(info.Addresses, a) && len(info.Addresses) < maxAddresses {
			info.Addresses = append(info.Addresses, a)
		}
	}
	if len(info.Addresses) == 0 && e.DiscoveryKey == "" {
		return peerInfo{}, peers.Enrolled{}, fmt.Errorf("%w: no address for it, and no DHT key", errHostUnreachable)
	}
	info.URL = "https://" + firstOr(info.Addresses, "journal-host.invalid:56398")
	dir := peerDir(config, info.key())
	if _, err := os.Stat(dir); err == nil {
		return peerInfo{}, peers.Enrolled{}, fmt.Errorf("peer %s is added already (%s)", e.Name, dir)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return peerInfo{}, peers.Enrolled{}, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: e.Name}}, key)
	if err != nil {
		return peerInfo{}, peers.Enrolled{}, err
	}
	body, _ := json.Marshal(peers.EnrollRequest{Code: strings.ToUpper(strings.TrimSpace(e.Code)), CSR: csr, Offer: e.Offer})
	// Dialed by every address the invite gave, then the DHT: a book of its own until the peer is kept.
	book := &peerBook{config: config, name: e.Name, fingerprint: e.Fingerprint, key: e.DiscoveryKey,
		addrs: slices.Clone(info.Addresses), unsaved: true}
	client := copies.PeerClient(tls.Certificate{}, e.Fingerprint, book)
	client.Timeout = time.Minute
	resp, err := client.Post(info.URL+"/peer/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		return peerInfo{}, peers.Enrolled{}, fmt.Errorf("%w: %v", errHostUnreachable, err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var refusal struct{ Error, Message string }
		json.Unmarshal(answer, &refusal)
		return peerInfo{}, peers.Enrolled{}, fmt.Errorf("%w: %s %s", errHostRefused, refusal.Error, refusal.Message)
	}
	var enrolled peers.Enrolled
	if err := json.Unmarshal(answer, &enrolled); err != nil {
		return peerInfo{}, peers.Enrolled{}, fmt.Errorf("the host's answer: %w", err)
	}
	cert, err := x509.ParseCertificate(enrolled.Certificate)
	if err != nil || !enroll.IsPeer(cert) {
		return peerInfo{}, peers.Enrolled{}, fmt.Errorf("the host's certificate isn't a peer's: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return peerInfo{}, peers.Enrolled{}, err
	}
	info.HostName, info.Quota, info.HostUser = enrolled.Name, enrolled.Quota, enrolled.Owner
	info.AddedAt, info.ExpiresAt = time.Now().UTC(), cert.NotAfter.UTC()
	info.Addresses = book.Addresses() // the one that answered first
	if d := enrolled.Discovery; d != nil {
		for _, a := range discovery.HostPorts(d.Addresses) {
			if !slices.Contains(info.Addresses, a) && len(info.Addresses) < maxAddresses {
				info.Addresses = append(info.Addresses, a)
			}
		}
		if d.Key != "" {
			info.DiscoveryKey = d.Key
		}
	}
	infoJSON, _ := json.MarshalIndent(info, "", "  ")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return peerInfo{}, peers.Enrolled{}, err
	}
	files := []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{"key.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600},
		{"cert.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: enrolled.Certificate}), 0o644},
		{"peer.json", infoJSON, 0o644}, // last: a folder without it isn't a peer (peerKeys)
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.name), f.data, f.mode); err != nil {
			os.RemoveAll(dir)
			return peerInfo{}, peers.Enrolled{}, err
		}
	}
	return info, enrolled, nil
}

func firstOr(list []string, otherwise string) string {
	if len(list) > 0 {
		return list[0]
	}
	return otherwise
}

// peerRevokeCommand stops a peer that keeps copies here: its certificate is refused, its files stay.
func peerRevokeCommand(config auth.AppConfig, name string) error {
	var data struct {
		RevokePeer struct {
			Name string  `json:"name"`
			Used float64 `json:"used"`
		} `json:"revokePeer"`
	}
	if err := adminQuery(config, `mutation($name: String!) { revokePeer(name: $name) { name used } }`,
		map[string]any{"name": name}, &data); err != nil {
		return err
	}
	fmt.Printf("Peer %s revoked. Its files (%s) stay in %s until you delete them.\n", name,
		copies.Size(int64(data.RevokePeer.Used)), filepath.Join(config.PeersFolder, name))
	return nil
}

// peerRemoveCommand stops this server using a peer: its folder in peers_dir is set aside (not deleted: its key can
// still read the files there). The files at the peer stay.
func peerRemoveCommand(config auth.AppConfig, name string) error {
	if _, _, err := loadPeer(config, name); err != nil {
		return err
	}
	for _, user := range config.Users {
		for _, cc := range user.Copies {
			if cc.Peer == name {
				return fmt.Errorf("user %s's copy goes to %s: take it out of config.json first", user.Username, name)
			}
		}
	}
	dir := peerDir(config, name)
	aside := dir + ".removed-" + time.Now().Format("2006-01-02-150405")
	if err := os.Rename(dir, aside); err != nil {
		return err
	}
	fmt.Printf("Peer %s removed; its key is kept in %s. The files at %s stay.\n", name, aside, name)
	return nil
}

// peersCommand lists both directions: the peers this server keeps copies on, and those that keep copies here.
func peersCommand(config auth.AppConfig) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "Our copies are kept on:")
	found := false
	for _, key := range peerKeys(config) {
		info, _, err := loadPeer(config, key)
		if err != nil {
			fmt.Fprintf(tw, "  %s\t%v\n", key, err)
			continue
		}
		found = true
		trust := "trusted"
		if !info.Trusted {
			trust = "not trusted"
		}
		var users []string
		for _, user := range config.Users {
			for _, cc := range user.Copies {
				if cc.Peer == info.Name && info.User == "" {
					users = append(users, user.Username)
				}
			}
		}
		usedBy := "none in config.json"
		if info.User != "" {
			usedBy = info.User + "'s own (a swap from the phone)"
		} else if len(users) > 0 {
			usedBy = strings.Join(users, ", ")
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\tquota %s\tcopies: %s\tcertificate until %s\n", info.Name, info.URL, trust,
			copies.Size(info.Quota), usedBy, info.ExpiresAt.Local().Format("2006-01-02"))
	}
	if !found {
		fmt.Fprintln(tw, "  none (journal-server peer add)")
	}
	fmt.Fprintln(tw, "\nWe keep copies for:")
	var data struct {
		Peers []struct {
			Name      string     `json:"name"`
			Owner     *string    `json:"owner"`
			Quota     float64    `json:"quota"`
			Used      float64    `json:"used"`
			AddedAt   time.Time  `json:"addedAt"`
			RevokedAt *time.Time `json:"revokedAt"`
		} `json:"peers"`
	}
	switch err := adminQuery(config, `{ peers { name owner quota used addedAt revokedAt } }`, nil, &data); {
	case err != nil:
		fmt.Fprintf(tw, "  (the server isn't running: %v)\n", err)
	case config.PeersFolder == "":
		fmt.Fprintln(tw, "  none (set peers_folder in config.json to be invited)")
	case len(data.Peers) == 0:
		fmt.Fprintln(tw, "  none (journal-server peer invite)")
	}
	for _, p := range data.Peers {
		state := "since " + p.AddedAt.Local().Format("2006-01-02")
		if p.RevokedAt != nil {
			state = "revoked " + p.RevokedAt.Local().Format("2006-01-02")
		}
		name := p.Name
		if p.Owner != nil {
			name = *p.Owner + "'s friend " + p.Name
		}
		fmt.Fprintf(tw, "  %s\t%s of %s\t%s\n", name, copies.Size(int64(p.Used)), copies.Size(int64(p.Quota)), state)
	}
	return tw.Flush()
}

// parseSize reads "2TB", "500 GB", "1.5T" (decimal units, as copies.Size prints them) or a number of bytes.
func parseSize(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "B"), " ")
	scale := 1.0
	if s != "" {
		if i := strings.IndexByte("KMGTP", s[len(s)-1]); i >= 0 {
			scale = 1e3
			for range i {
				scale *= 1e3
			}
			s = strings.TrimSpace(s[:len(s)-1])
		}
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("a size like 2TB")
	}
	return int64(n * scale), nil
}

// openPeerHost serves other servers' copies from peers_folder (docs/peers.md), or nothing without it.
func openPeerHost(config auth.AppConfig, issuer *enroll.Issuer) (*peers.Host, error) {
	if config.PeersFolder == "" {
		return nil, nil
	}
	if info, err := os.Stat(config.PeersFolder); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("peers_folder %s must be a folder that exists", config.PeersFolder)
	}
	registry, err := peers.OpenRegistry(config.PeersFile)
	if err != nil {
		return nil, err
	}
	host := peers.NewHost(config.PeersFolder, registry, issuer, time.Now)
	host.Users = map[string]bool{}
	for _, u := range config.Users {
		host.Users[u.Username] = true
	}
	return host, nil
}

// renewPeersDaily renews this server's peer certificates that expire within enroll.RenewBefore: now, and daily
// until ctx ends. The copies to a peer switch to the new certificate at once.
func renewPeersDaily(ctx context.Context, config auth.AppConfig, copiers func() []*copies.Copier) {
	for {
		renewPeers(config, copiers(), time.Now())
		select {
		case <-ctx.Done():
			return
		case <-time.After(24 * time.Hour):
		}
	}
}

// renewPeers renews each added peer's certificate that expires within enroll.RenewBefore of now.
func renewPeers(config auth.AppConfig, copiers []*copies.Copier, now time.Time) {
	live := map[string][]*copies.Peer{}
	for _, c := range copiers {
		switch t := c.Target.(type) {
		case *copies.Peer:
			live[t.Key] = append(live[t.Key], t)
		case *copies.Encrypted:
			live[t.Inner.Key] = append(live[t.Inner.Key], t.Inner)
		}
	}
	for _, key := range peerKeys(config) {
		info, cert, err := loadPeer(config, key)
		if err != nil || info.ExpiresAt.Sub(now) > enroll.RenewBefore {
			continue
		}
		renewed, err := renewPeer(config, info, cert)
		if err != nil {
			log.Printf("Peer %s: renewing the certificate (until %s): %v", info.Name, info.ExpiresAt.Format("2006-01-02"), err)
			continue
		}
		for _, p := range live[info.key()] {
			p.SetCertificate(renewed)
		}
		log.Printf("Peer %s: certificate renewed", info.Name)
	}
}

// renewPeer gets a new certificate for a new key from the peer, and keeps both in its folder in place of the old.
func renewPeer(config auth.AppConfig, info peerInfo, cert tls.Certificate) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: info.Name}}, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	der, err := peerFor(config, info, "", cert).Renew(csr)
	if err != nil {
		return tls.Certificate{}, err
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil || !enroll.IsPeer(parsed) {
		return tls.Certificate{}, fmt.Errorf("the host's certificate isn't a peer's: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	dir := peerDir(config, info.key())
	// The old pair is kept until the new is in place, so a stop between the two renames leaves a pair that works.
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	for name, data := range map[string][]byte{"key.pem.new": keyPEM, "cert.pem.new": certPEM} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return tls.Certificate{}, err
		}
	}
	for _, name := range []string{"key.pem", "cert.pem"} {
		os.Remove(filepath.Join(dir, name+".prev"))
		if err := os.Rename(filepath.Join(dir, name), filepath.Join(dir, name+".prev")); err != nil {
			return tls.Certificate{}, err
		}
	}
	for _, name := range []string{"key.pem", "cert.pem"} {
		if err := os.Rename(filepath.Join(dir, name+".new"), filepath.Join(dir, name)); err != nil {
			return tls.Certificate{}, err
		}
	}
	updatePeerInfo(config, info.key(), func(i *peerInfo) { i.ExpiresAt = parsed.NotAfter.UTC() })
	return tls.X509KeyPair(certPEM, keyPEM)
}
