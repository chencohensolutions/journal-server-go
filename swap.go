package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/copies"
	"github.com/journal/server-go/discovery"
	"github.com/journal/server-go/graph/model"
	"github.com/journal/server-go/graph/resolver"
	"github.com/journal/server-go/media"
	"github.com/journal/server-go/peers"
)

// Swaps set up from the phones (docs/peers.md, "Swaps from the phones"). Each user sets up their own, with no
// command line, no config.json edit and no restart:
//
//   - On the host, swapInvite makes an invite that belongs to the user (peers.Host.Invite with an owner): the peer
//     it lets in keeps its files in peers_folder/<user>/<friend>/, and only that user sees it or revokes it.
//   - On the other server, joinSwap enrolls with the host as peer add does, and keeps the peer as the user's own
//     (peers_dir/<user>/<friend>/, with the copy in its peer.json), then starts the user's copy there.
//   - One scan, both ways: joinSwap may offer the host's user a copy here in return, in the same enrollment. The
//     host keeps the offer for 10 minutes, its user's phone shows it, and accepting joins back.
//
// A phone acts only on its own user: no call here reads or changes another user's peers, copies or invites.

// swapService is resolver.SwapService.
type swapService struct {
	config  auth.AppConfig
	library *media.Library
	host    *peers.Host // nil without peers_folder
	set     *copySet
	// This server's fingerprint and where it is, for the links it makes.
	self func() (string, peers.Discovery)
	now  func() time.Time

	joinMu sync.Mutex // one join at a time: names and copy keys are checked, then made

	mu     sync.Mutex
	offers map[string]swapOffer // by id
}

// swapOffer is a swap a friend's server offered back to owner as it joined owner's invite (named name here).
type swapOffer struct {
	id, owner, name string
	offer           peers.Offer
}

func newSwapService(config auth.AppConfig, library *media.Library, host *peers.Host, set *copySet,
	self func() (string, peers.Discovery)) *swapService {
	s := &swapService{config: config, library: library, host: host, set: set, self: self, now: time.Now,
		offers: map[string]swapOffer{}}
	if host != nil {
		host.Offered = s.offered
	}
	return s
}

// selfFromDiscovery is this server's fingerprint and addresses for a link, from the running server's discovery
// (thisServer), or from its certificate and public_url before that is known.
func selfFromDiscovery(config auth.AppConfig) func() (string, peers.Discovery) {
	return func() (string, peers.Discovery) {
		if d := thisServer.Load(); d != nil {
			return d.fingerprint, d.forPeers()
		}
		fingerprint, _ := serverFingerprint(config)
		var found peers.Discovery
		if config.PublicURL != "" {
			found.Addresses = []string{config.PublicURL}
		}
		return fingerprint, found
	}
}

// maxLinkAddresses is the most addresses a swap link carries, as the enrollment link.
const maxLinkAddresses = 6

// swapLink is what a swap invite's QR code holds: journal://swap?code=…&id=…&key=…&address=…&from=…
func swapLink(code, fingerprint string, found peers.Discovery, from string) string {
	query := url.Values{"code": {code}, "id": {discovery.FormatID(fingerprint)}, "from": {from}}
	if found.Key != "" {
		query.Set("key", found.Key)
	}
	for _, a := range found.Addresses[:min(len(found.Addresses), maxLinkAddresses)] {
		query.Add("address", a)
	}
	return "journal://swap?" + query.Encode()
}

// parsedLink is a swap link read back.
type parsedLink struct {
	code, fingerprint, key, from string
	addresses                    []string // host:port
}

func parseSwapLink(link string) (parsedLink, error) {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || u.Scheme != "journal" || u.Host != "swap" {
		return parsedLink{}, errors.New("not a swap link (journal://swap?…)")
	}
	q := u.Query()
	fingerprint, err := discovery.ParseID(q.Get("id"))
	if err != nil {
		return parsedLink{}, fmt.Errorf("the link's server id: %w", err)
	}
	p := parsedLink{code: q.Get("code"), fingerprint: fingerprint, key: q.Get("key"), from: q.Get("from"),
		addresses: discovery.HostPorts(q["address"])}
	if p.code == "" {
		return parsedLink{}, errors.New("the link has no code")
	}
	if p.key != "" {
		if _, err := discovery.ParseKey(p.key); err != nil {
			return parsedLink{}, fmt.Errorf("the link's DHT key: %w", err)
		}
	}
	if len(p.addresses) == 0 && p.key == "" {
		return parsedLink{}, errors.New("the link says nowhere to find the server")
	}
	return p, nil
}

func (s *swapService) Info(user string) *model.SwapInfo {
	info := &model.SwapInfo{Available: s.host != nil, CopyKey: s.copyKeyKind(user)}
	if s.host != nil {
		if free, err := s.host.FreeSpace(); err == nil {
			info.FreeSpace = &free
		}
	}
	return info
}

func (s *swapService) copyKeyKind(user string) model.CopyKeyKind {
	if _, err := os.Stat(copyKeyFile(s.config, user)); err != nil {
		return model.CopyKeyKindNone
	}
	key, err := loadCopyKey(s.config, user)
	if err == nil && key.Derived != nil {
		return model.CopyKeyKindPassphrase
	}
	return model.CopyKeyKindKeyFile
}

func (s *swapService) Invite(user, name string, quota int64) (*model.SwapInvite, error) {
	if s.host == nil {
		return nil, errors.New("this server keeps no friends' copies: its admin sets peers_folder in config.json")
	}
	if !peers.ValidName(name) {
		return nil, fmt.Errorf("%q can't name a friend: letters, digits and dashes, up to 64", name)
	}
	if free, err := s.host.FreeSpace(); err == nil && quota > free {
		return nil, fmt.Errorf("%s is more than the %s free", copies.Size(quota), copies.Size(free))
	}
	code, expires, err := s.host.Invite(user, name, quota)
	if errors.Is(err, peers.ErrNameTaken) {
		return nil, fmt.Errorf("you keep the copy of a friend named %s already", name)
	}
	if err != nil {
		return nil, err
	}
	fingerprint, found := s.self()
	log.Printf("User %s invited a friend's server (%s) to keep its copy here, up to %s", user, name, copies.Size(quota))
	return &model.SwapInvite{Link: swapLink(code, fingerprint, found, user), Code: code, ExpiresAt: expires}, nil
}

func refused(reason model.SwapRefusal, format string, args ...any) (resolver.JoinOutcome, error) {
	return resolver.JoinOutcome{Refused: &model.SwapRefused{Reason: reason, Message: fmt.Sprintf(format, args...)}}, nil
}

func (s *swapService) Join(ctx context.Context, user string, join resolver.SwapJoin) (resolver.JoinOutcome, error) {
	s.joinMu.Lock()
	defer s.joinMu.Unlock()
	store := s.library.Store(user)
	if store == nil {
		return resolver.JoinOutcome{}, fmt.Errorf("%s has no data folder", user)
	}
	link, err := parseSwapLink(join.Link)
	if err != nil {
		return refused(model.SwapRefusalInvalidLink, "%v", err)
	}
	if own, _ := s.self(); own == link.fingerprint {
		return refused(model.SwapRefusalOwnServer, "the link is this server's own: a swap is with a friend's server")
	}
	name := join.Name
	if !peers.ValidName(name) {
		return resolver.JoinOutcome{}, fmt.Errorf("%q can't name a friend: letters, digits and dashes, up to 64", name)
	}
	if s.nameTaken(user, name) {
		return refused(model.SwapRefusalNameTaken, "you have a copy named %s already", name)
	}
	for _, info := range s.mine(user) {
		if info.Fingerprint == link.fingerprint && (info.HostUser == "" || link.from == "" || info.HostUser == link.from) {
			return refused(model.SwapRefusalAlreadySwapped, "your copy is at that friend's server already, as %s", info.Name)
		}
	}

	// The copy key: the user's, checked against the passphrase, or a new one from it.
	var newKey *copies.CopyKey
	if !join.Plain {
		passphrase := ""
		if join.Passphrase != nil {
			passphrase = *join.Passphrase
		}
		switch s.copyKeyKind(user) {
		case model.CopyKeyKindKeyFile:
		case model.CopyKeyKindPassphrase:
			if passphrase == "" {
				return refused(model.SwapRefusalNeedsPassphrase, "your copies are encrypted with a passphrase: give it")
			}
			stored, err := loadCopyKey(s.config, user)
			if err != nil {
				return resolver.JoinOutcome{}, err
			}
			key, _, err := copies.DeriveKey(user, passphrase, *stored.Derived, stored.CreatedAt)
			if err != nil {
				return resolver.JoinOutcome{}, err
			}
			if key.Recipient != stored.Recipient {
				return refused(model.SwapRefusalWrongPassphrase, "that isn't the passphrase of your copies")
			}
		default:
			if passphrase == "" {
				return refused(model.SwapRefusalNeedsPassphrase, "an encrypted copy needs a passphrase")
			}
			if weak := copies.WeakPassphrase(passphrase); weak != "" {
				return refused(model.SwapRefusalWeakPassphrase, "too weak: %s", weak)
			}
			key, _, err := copies.DeriveKey(user, passphrase, copies.NewDerivation(), s.now())
			if err != nil {
				return resolver.JoinOutcome{}, err
			}
			newKey = &key
		}
	}

	// One scan, both ways: an invite for the friend's user, sent in the enrollment.
	var offer *peers.Offer
	if join.OfferQuota > 0 {
		free := int64(0)
		if s.host != nil {
			free, _ = s.host.FreeSpace()
		}
		if s.host == nil || join.OfferQuota > free {
			return refused(model.SwapRefusalNoSpace, "this server can keep %s for a friend at most", copies.Size(free))
		}
		code, expires, err := s.host.Invite(user, name, join.OfferQuota)
		if errors.Is(err, peers.ErrNameTaken) {
			return refused(model.SwapRefusalNameTaken, "you keep the copy of a friend named %s already", name)
		}
		if err != nil {
			return resolver.JoinOutcome{}, err
		}
		fingerprint, found := s.self()
		offer = &peers.Offer{Link: swapLink(code, fingerprint, found, user), Quota: join.OfferQuota, From: user,
			ExpiresAt: expires}
	}

	info, _, err := enrollPeer(s.config, peerEnrollment{Name: name, User: user, Addresses: link.addresses,
		DiscoveryKey: link.key, Code: link.code, Fingerprint: link.fingerprint, Trusted: join.Plain,
		Copy: &swapCopy{Encrypt: !join.Plain, At: auth.DefaultCopyAt}, Offer: offer})
	switch {
	case errors.Is(err, errHostUnreachable):
		return refused(model.SwapRefusalUnreachable, "%v", err)
	case errors.Is(err, errHostRefused):
		return refused(model.SwapRefusalCodeRefused, "%v", err)
	case err != nil:
		return resolver.JoinOutcome{}, err
	}
	if newKey != nil {
		if err := storeCopyKey(s.config, *newKey); err != nil {
			return resolver.JoinOutcome{}, err
		}
	}
	swap := resolver.Swap{Name: info.Name, Quota: info.Quota, AddedAt: info.AddedAt, Encrypted: !join.Plain}
	c, err := swapCopier(s.config, store, info)
	if err != nil {
		log.Printf("User %s's copy at %s: %v", user, name, err)
	} else {
		s.set.add(c)
		swap.Copier = c
	}
	how := "encrypted"
	if join.Plain {
		how = "plain"
	}
	log.Printf("User %s keeps their copy at a friend's server (%s, %s, up to %s)", user, name, how, copies.Size(info.Quota))
	return resolver.JoinOutcome{Swap: &swap, Offered: offer != nil}, nil
}

// nameTaken says whether user has a peer or a copy named name.
func (s *swapService) nameTaken(user, name string) bool {
	if _, err := os.Stat(peerDir(s.config, peerKey(user, name))); err == nil {
		return true
	}
	for _, c := range s.set.all() {
		if c.User == user && strings.EqualFold(c.Folder, name) {
			return true
		}
	}
	return false
}

// offered keeps the swap a peer's server offered back as it joined p's owner's invite (peers.Host.Offered).
func (s *swapService) offered(p peers.Peer, offer peers.Offer) {
	if !strings.HasPrefix(offer.Link, "journal://swap?") || offer.Quota <= 0 {
		return
	}
	if len(offer.From) > 64 {
		offer.From = offer.From[:64]
	}
	raw := make([]byte, 12)
	rand.Read(raw)
	id := hex.EncodeToString(raw)
	if offer.ExpiresAt.IsZero() || offer.ExpiresAt.After(s.now().Add(10*time.Minute)) {
		offer.ExpiresAt = s.now().Add(10 * time.Minute)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offers[id] = swapOffer{id: id, owner: p.Owner, name: p.Name, offer: offer}
	log.Printf("User %s's friend %s offers to keep their copy too (%s)", p.Owner, p.Name, copies.Size(offer.Quota))
}

// takeOffer is user's offer id, taken so it is answered once.
func (s *swapService) takeOffer(user, id string) (swapOffer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.offers[id]
	if !ok || o.owner != user {
		return swapOffer{}, false
	}
	delete(s.offers, id)
	return o, s.now().Before(o.offer.ExpiresAt)
}

func (s *swapService) Accept(ctx context.Context, user, id string, passphrase *string, plain bool) (resolver.JoinOutcome, error) {
	o, ok := s.takeOffer(user, id)
	if !ok {
		return refused(model.SwapRefusalOfferGone, "the offer isn't there any more: it lasts 10 minutes")
	}
	outcome, err := s.Join(ctx, user, resolver.SwapJoin{Link: o.offer.Link, Name: o.name, Passphrase: passphrase, Plain: plain})
	if err == nil && outcome.Refused != nil {
		switch outcome.Refused.Reason {
		case model.SwapRefusalNeedsPassphrase, model.SwapRefusalWeakPassphrase, model.SwapRefusalWrongPassphrase:
			s.mu.Lock()
			s.offers[id] = o // the code wasn't used: try again with the passphrase
			s.mu.Unlock()
		}
	}
	return outcome, err
}

func (s *swapService) Decline(user, id string) bool {
	_, ok := s.takeOffer(user, id)
	return ok
}

func (s *swapService) Offers(user string) []*model.SwapOffer {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := []*model.SwapOffer{}
	for id, o := range s.offers {
		if !s.now().Before(o.offer.ExpiresAt) {
			delete(s.offers, id)
			continue
		}
		if o.owner == user {
			list = append(list, &model.SwapOffer{ID: id, From: o.offer.From, Name: o.name, Quota: o.offer.Quota,
				ExpiresAt: o.offer.ExpiresAt})
		}
	}
	slices.SortFunc(list, func(a, b *model.SwapOffer) int { return a.ExpiresAt.Compare(b.ExpiresAt) })
	return list
}

// mine are user's own peers.
func (s *swapService) mine(user string) []peerInfo {
	var list []peerInfo
	for _, key := range peerKeys(s.config) {
		if info, _, err := loadPeer(s.config, key); err == nil && info.User == user {
			list = append(list, info)
		}
	}
	return list
}

// copierOn is user's copy on the peer with that key.
func (s *swapService) copierOn(user, key string) *copies.Copier {
	for _, c := range s.set.all() {
		if c.User != user {
			continue
		}
		switch t := c.Target.(type) {
		case *copies.Peer:
			if t.Key == key {
				return c
			}
		case *copies.Encrypted:
			if t.Inner.Key == key {
				return c
			}
		}
	}
	return nil
}

func (s *swapService) Mine(user string) []resolver.Swap {
	var list []resolver.Swap
	for _, info := range s.mine(user) {
		list = append(list, resolver.Swap{Name: info.Name, Quota: info.Quota, AddedAt: info.AddedAt,
			Encrypted: info.Copy != nil && info.Copy.Encrypt, Copier: s.copierOn(user, info.key())})
	}
	return list
}

func (s *swapService) Kept(user string) []*model.KeptSwap {
	list := []*model.KeptSwap{}
	if s.host == nil {
		return list
	}
	// One line per friend: the active peer, else the last revoked one whose files are still here.
	latest := map[string]peers.Peer{}
	var names []string
	for _, p := range s.host.Registry.Of(user) {
		before, seen := latest[p.Name]
		if !seen {
			names = append(names, p.Name)
		}
		if !seen || before.RevokedAt != nil {
			latest[p.Name] = p
		}
	}
	for _, name := range names {
		p := latest[name]
		if p.RevokedAt != nil && !s.host.Holds(p) {
			continue
		}
		list = append(list, &model.KeptSwap{Name: p.Name, Quota: p.Quota, Used: s.host.Used(p), AddedAt: p.AddedAt,
			RevokedAt: p.RevokedAt})
	}
	return list
}

func (s *swapService) Remove(user, name string) error {
	key := peerKey(user, name)
	info, _, err := loadPeer(s.config, key)
	if err != nil || info.User != user {
		return fmt.Errorf("you have no copy at a friend named %q", name)
	}
	if c := s.copierOn(user, key); c != nil {
		s.set.remove(c)
	}
	forgetBook(s.config, info)
	dir := peerDir(s.config, key)
	if err := os.Rename(dir, dir+".removed-"+s.now().Format("2006-01-02-150405")); err != nil {
		return err
	}
	log.Printf("User %s stopped keeping their copy at %s; its key is kept in %s.removed-…", user, name, filepath.Base(dir))
	return nil
}

func (s *swapService) Revoke(user, name string, deleteFiles bool) error {
	if s.host == nil {
		return errors.New("this server keeps no friends' copies")
	}
	_, err := s.host.Registry.RevokeOf(user, name, s.now().UTC())
	if errors.Is(err, peers.ErrPeerNotFound) && deleteFiles {
		err = nil // revoked before: only the files go now
	}
	if err != nil {
		return fmt.Errorf("you keep no copy of a friend named %q", name)
	}
	if deleteFiles {
		if err := s.host.Remove(user, name); err != nil {
			return err
		}
	}
	log.Printf("User %s stopped keeping the copy of their friend %s (files deleted: %v)", user, name, deleteFiles)
	return nil
}
