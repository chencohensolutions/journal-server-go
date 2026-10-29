package main

import (
	"bytes"
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
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/copies"
	"github.com/journal/server-go/enroll"
	"github.com/journal/server-go/peers"
)

// A peer this server keeps copies on is a folder in peers_dir (certs/peers/<name>/): key.pem and cert.pem (the
// client certificate the host signed) and peer.json. See docs/peers.md.

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
}

func peerDir(config auth.AppConfig, name string) string { return filepath.Join(config.PeersDir, name) }

// loadPeer reads a peer added with peer add.
func loadPeer(config auth.AppConfig, name string) (peerInfo, tls.Certificate, error) {
	dir := peerDir(config, name)
	data, err := os.ReadFile(filepath.Join(dir, "peer.json"))
	if errors.Is(err, os.ErrNotExist) {
		return peerInfo{}, tls.Certificate{}, fmt.Errorf("no peer %q: add it with journal-server peer add", name)
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
		return peerInfo{}, tls.Certificate{}, fmt.Errorf("peer %s: %w", name, err)
	}
	return info, cert, nil
}

// peerTarget is the copy of user's data folder on the peer cc names. A plain copy goes only to a trusted peer.
func peerTarget(config auth.AppConfig, user string, cc auth.CopyConfig) (*copies.Peer, error) {
	info, cert, err := loadPeer(config, cc.Peer)
	if err != nil {
		return nil, err
	}
	if !info.Trusted {
		return nil, fmt.Errorf("peer %s isn't trusted, and encrypted copies aren't built yet: add it again with --trusted to keep a plain copy there", cc.Peer)
	}
	p := copies.NewPeer(info.Name, info.URL, user, cert, info.Fingerprint)
	p.BytesPerSecond = int64(cc.BandwidthKbps) * 1000 / 8
	return p, nil
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
		address = "https://<this server's address>:" + config.Port
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
	if !strings.HasPrefix(address, "https://") {
		return fmt.Errorf("the host's address starts with https://, not %q", address)
	}
	code, fingerprint, ok := strings.Cut(invite, ".")
	if !ok || len(fingerprint) != 64 {
		return errors.New("the code is the whole of what peer invite printed: the code, a dot and the server's fingerprint")
	}
	dir := peerDir(config, name)
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("peer %s is added already (%s); peer remove it first", name, dir)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: name}}, key)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(peers.EnrollRequest{Code: strings.ToUpper(strings.TrimSpace(code)), CSR: csr})
	client := copies.PeerClient(tls.Certificate{}, strings.ToLower(fingerprint))
	client.Timeout = time.Minute
	resp, err := client.Post(address+"/peer/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("reaching %s: %w", address, err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var refusal struct{ Error, Message string }
		json.Unmarshal(answer, &refusal)
		return fmt.Errorf("the host refused: %s %s", refusal.Error, refusal.Message)
	}
	var enrolled peers.Enrolled
	if err := json.Unmarshal(answer, &enrolled); err != nil {
		return fmt.Errorf("the host's answer: %w", err)
	}
	cert, err := x509.ParseCertificate(enrolled.Certificate)
	if err != nil || !enroll.IsPeer(cert) {
		return fmt.Errorf("the host's certificate isn't a peer's: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	info := peerInfo{V: 1, Name: name, URL: address, Fingerprint: strings.ToLower(fingerprint), Trusted: trusted,
		HostName: enrolled.Name, Quota: enrolled.Quota, AddedAt: time.Now().UTC(), ExpiresAt: cert.NotAfter.UTC()}
	infoJSON, _ := json.MarshalIndent(info, "", "  ")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	files := []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{"key.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600},
		{"cert.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: enrolled.Certificate}), 0o644},
		{"peer.json", infoJSON, 0o644},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.name), f.data, f.mode); err != nil {
			return err
		}
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
	return nil
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
	entries, _ := os.ReadDir(config.PeersDir)
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "Our copies are kept on:")
	found := false
	for _, e := range entries {
		if !e.IsDir() || strings.Contains(e.Name(), ".removed-") {
			continue
		}
		info, _, err := loadPeer(config, e.Name())
		if err != nil {
			fmt.Fprintf(tw, "  %s\t%v\n", e.Name(), err)
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
				if cc.Peer == info.Name {
					users = append(users, user.Username)
				}
			}
		}
		usedBy := "none in config.json"
		if len(users) > 0 {
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
			Quota     float64    `json:"quota"`
			Used      float64    `json:"used"`
			AddedAt   time.Time  `json:"addedAt"`
			RevokedAt *time.Time `json:"revokedAt"`
		} `json:"peers"`
	}
	switch err := adminQuery(config, `{ peers { name quota used addedAt revokedAt } }`, nil, &data); {
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
		fmt.Fprintf(tw, "  %s\t%s of %s\t%s\n", p.Name, copies.Size(int64(p.Used)), copies.Size(int64(p.Quota)), state)
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
	return peers.NewHost(config.PeersFolder, registry, issuer, time.Now), nil
}
