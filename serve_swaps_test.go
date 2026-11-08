package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/copies"
)

// swapEnv is a server whose users may swap: peers_folder set, and every folder in the test's temp dirs.
func swapEnv(t *testing.T, users ...string) *testEnv {
	t.Helper()
	return newTestEnv(t, func(c *auth.AppConfig) {
		c.PeersFolder, c.PeersFile = t.TempDir(), filepath.Join(t.TempDir(), "peers.json")
		c.PeersDir, c.CopyKeysDir = t.TempDir(), t.TempDir()
		c.Users[0].Username = users[0]
		for _, u := range users[1:] {
			c.Users = append(c.Users, auth.UserConfig{Username: u, DataFolder: filepath.Join(t.TempDir(), u)})
		}
	})
}

// fastDerivation makes keys from passphrases quickly, for the test's length.
func fastDerivation(t *testing.T) {
	before := copies.DefaultDerivation
	copies.DefaultDerivation = copies.Derivation{Time: 1, MemoryKiB: 8 << 10, Threads: 1}
	t.Cleanup(func() { copies.DefaultDerivation = before })
}

func gqlData[T any](t *testing.T, resp gqlResponse, field string) T {
	t.Helper()
	var out T
	if len(resp.Errors) > 0 {
		t.Fatalf("%s: %+v", field, resp.Errors)
	}
	if err := json.Unmarshal(resp.Data[field], &out); err != nil {
		t.Fatalf("%s: %v in %s", field, err, resp.Data[field])
	}
	return out
}

const joinSwapMutation = `mutation($link: String!, $name: String!, $passphrase: String, $plain: Boolean, $offer: Int64) {
	joinSwap(link: $link, name: $name, passphrase: $passphrase, plain: $plain, offerQuota: $offer) {
		__typename
		... on SwapJoined { offered swap { name quota encrypted copy { folder kind encrypted } } }
		... on SwapRefused { reason message }
	}
}`

type joinResult struct {
	Typename string `json:"__typename"`
	Offered  bool
	Swap     struct {
		Name      string
		Quota     int64
		Encrypted bool
		Copy      *struct {
			Folder, Kind string
			Encrypted    bool
		}
	}
	Reason, Message string
}

const passphrase = "correct horse battery staple violin moon"

// A swap set up from the two phones, with no command line: dana invites hen's server from her phone, hen's phone
// scans it and offers dana a copy back, dana's phone accepts. Each copy is encrypted, at the other's server, in a
// folder of its own user; nobody else on either server sees or changes it.
func TestASwapSetUpFromThePhones(t *testing.T) {
	fastDerivation(t)
	dana := swapEnv(t, "dana", "ann")
	hen := swapEnv(t, "hen")
	danaPhone, annPhone, henPhone := dana.enrolledClientOf(t, "dana"), dana.enrolledClientOf(t, "ann"), hen.enrolledClient(t)

	// dana's phone sees the server keeps friends' copies, and how much room there is.
	type swapInfo struct {
		Available bool
		FreeSpace *int64
		CopyKey   string
	}
	var si struct{ Swap swapInfo }
	json.Unmarshal(dana.graphql(t, danaPhone, `{ serverInfo { swap { available freeSpace copyKey } } }`, nil).Data["serverInfo"], &si)
	if !si.Swap.Available || si.Swap.FreeSpace == nil || *si.Swap.FreeSpace <= 0 || si.Swap.CopyKey != "NONE" {
		t.Fatalf("serverInfo.swap: %+v", si.Swap)
	}
	tooMuch := *si.Swap.FreeSpace + 1
	if resp := dana.graphql(t, danaPhone, `mutation($q: Int64!) { swapInvite(name: "hen", quota: $q) { link } }`,
		map[string]any{"q": tooMuch}); len(resp.Errors) == 0 {
		t.Error("an invite for more than the free space")
	}

	type invite struct{ Link, Code string }
	inv := gqlData[invite](t, dana.graphql(t, danaPhone, `mutation { swapInvite(name: "hen", quota: 1000000000) { link code } }`, nil), "swapInvite")
	if !strings.HasPrefix(inv.Link, "journal://swap?") || !strings.Contains(inv.Link, "from=dana") || !strings.Contains(inv.Link, "address=") {
		t.Fatalf("the link: %s", inv.Link)
	}

	// hen's phone: a weak passphrase, then the link of hen's own server, are refused before anything is used.
	join := func(vars map[string]any) joinResult {
		t.Helper()
		return gqlData[joinResult](t, hen.graphql(t, henPhone, joinSwapMutation, vars), "joinSwap")
	}
	if r := join(map[string]any{"link": inv.Link, "name": "dana", "passphrase": "short one"}); r.Reason != "WEAK_PASSPHRASE" {
		t.Errorf("a weak passphrase: %+v", r)
	}
	if r := join(map[string]any{"link": inv.Link, "name": "dana"}); r.Reason != "NEEDS_PASSPHRASE" {
		t.Errorf("no passphrase: %+v", r)
	}
	if r := join(map[string]any{"link": "journal://enroll?code=x", "name": "dana", "passphrase": passphrase}); r.Reason != "INVALID_LINK" {
		t.Errorf("not a swap link: %+v", r)
	}
	ownInvite := gqlData[invite](t, hen.graphql(t, henPhone, `mutation { swapInvite(name: "x", quota: 1000) { link } }`, nil), "swapInvite")
	if r := join(map[string]any{"link": ownInvite.Link, "name": "me", "passphrase": passphrase}); r.Reason != "OWN_SERVER" {
		t.Errorf("hen's own link: %+v", r)
	}

	// Joined, encrypted, with a copy offered back.
	r := join(map[string]any{"link": inv.Link, "name": "dana", "passphrase": passphrase, "offer": 500000000})
	if r.Typename != "SwapJoined" || !r.Offered || r.Swap.Name != "dana" || r.Swap.Quota != 1000000000 || !r.Swap.Encrypted ||
		r.Swap.Copy == nil || r.Swap.Copy.Kind != "PEER" || !r.Swap.Copy.Encrypted {
		t.Fatalf("joined: %+v", r)
	}
	if r := join(map[string]any{"link": inv.Link, "name": "dana2", "passphrase": passphrase}); r.Reason != "ALREADY_SWAPPED" {
		t.Errorf("the same friend under another name: %+v", r)
	}
	if r := join(map[string]any{"link": inv.Link, "name": "dana", "passphrase": passphrase}); r.Reason != "NAME_TAKEN" {
		t.Errorf("the same name again: %+v", r)
	}
	// The same friend again, under another name: refused, one swap per friend.
	twice := gqlData[invite](t, dana.graphql(t, danaPhone, `mutation { swapInvite(name: "hen2", quota: 1000) { link } }`, nil), "swapInvite")
	if r := join(map[string]any{"link": twice.Link, "name": "dana-again", "passphrase": passphrase}); r.Reason != "ALREADY_SWAPPED" {
		t.Errorf("the same friend twice: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(hen.config.PeersDir, "hen", "dana", "peer.json")); err != nil {
		t.Errorf("the peer isn't hen's own: %v", err)
	}

	// hen's copy goes to dana's server, encrypted, in dana's friends' folder.
	hen.uploadPhoto(t, henPhone, []byte("a photo of hen's"), "IMG_1.jpg")
	c := hen.swaps.copierOn("hen", peerKey("hen", "dana"))
	if c == nil {
		t.Fatal("no copier for the swap")
	}
	runCopy(context.Background(), c)
	blobs := 0
	filepath.WalkDir(filepath.Join(dana.config.PeersFolder, "dana", "hen", "hen", "blobs"), func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			blobs++
		}
		return nil
	})
	if blobs == 0 {
		t.Fatal("no encrypted files at dana's server")
	}

	// hen's phone lists the swap; the copy shows among hen's copies too.
	type swaps struct {
		Mine []struct {
			Name      string
			Encrypted bool
			Copy      *struct{ Copied int }
		}
		Kept []struct {
			Name        string
			Quota, Used int64
		}
		Offers []struct {
			ID, From, Name string
			Quota          int64
		}
	}
	const swapsQuery = `{ swaps { mine { name encrypted copy { copied } } kept { name quota used } offers { id from name quota } } }`
	hs := gqlData[swaps](t, hen.graphql(t, henPhone, swapsQuery, nil), "swaps")
	if len(hs.Mine) != 1 || hs.Mine[0].Name != "dana" || hs.Mine[0].Copy == nil || hs.Mine[0].Copy.Copied != 1 {
		t.Errorf("hen's swaps: %+v", hs)
	}

	// dana's phone: hen's copy kept, and hen's offer to keep dana's. ann, on the same server, sees none of it.
	ds := gqlData[swaps](t, dana.graphql(t, danaPhone, swapsQuery, nil), "swaps")
	if len(ds.Kept) != 1 || ds.Kept[0].Name != "hen" || ds.Kept[0].Used == 0 || len(ds.Offers) != 1 ||
		ds.Offers[0].From != "hen" || ds.Offers[0].Name != "hen" || ds.Offers[0].Quota != 500000000 {
		t.Fatalf("dana's swaps: %+v", ds)
	}
	as := gqlData[swaps](t, dana.graphql(t, annPhone, swapsQuery, nil), "swaps")
	if len(as.Mine)+len(as.Kept)+len(as.Offers) != 0 {
		t.Errorf("ann sees dana's swaps: %+v", as)
	}
	accept := `mutation($id: ID!, $p: String) { acceptSwapOffer(id: $id, passphrase: $p) {
		__typename ... on SwapJoined { swap { name encrypted } } ... on SwapRefused { reason } } }`
	if r := gqlData[joinResult](t, dana.graphql(t, annPhone, accept, map[string]any{"id": ds.Offers[0].ID, "p": passphrase}), "acceptSwapOffer"); r.Reason != "OFFER_GONE" {
		t.Errorf("ann accepted dana's offer: %+v", r)
	}
	if resp := dana.graphql(t, annPhone, `mutation { revokeSwap(name: "hen") }`, nil); len(resp.Errors) == 0 {
		t.Error("ann revoked dana's friend")
	}
	if r := gqlData[joinResult](t, dana.graphql(t, danaPhone, accept, map[string]any{"id": ds.Offers[0].ID}), "acceptSwapOffer"); r.Reason != "NEEDS_PASSPHRASE" {
		t.Errorf("accepted without a passphrase: %+v", r)
	}
	daPass := "dana keeps her own six words here"
	if r := gqlData[joinResult](t, dana.graphql(t, danaPhone, accept, map[string]any{"id": ds.Offers[0].ID, "p": daPass}), "acceptSwapOffer"); r.Typename != "SwapJoined" || r.Swap.Name != "hen" || !r.Swap.Encrypted {
		t.Fatalf("dana accepts: %+v", r)
	}
	if hk := gqlData[swaps](t, hen.graphql(t, henPhone, swapsQuery, nil), "swaps"); len(hk.Kept) != 1 || hk.Kept[0].Name != "dana" || hk.Kept[0].Quota != 500000000 {
		t.Errorf("hen keeps dana's copy: %+v", hk.Kept)
	}
	json.Unmarshal(dana.graphql(t, danaPhone, `{ serverInfo { swap { copyKey } } }`, nil).Data["serverInfo"], &si)
	if si.Swap.CopyKey != "PASSPHRASE" {
		t.Errorf("dana's copy key: %+v", si.Swap)
	}

	// Started again, each server opens its user's swap copy by itself (no config.json).
	again, err := newCopiers(hen.config, hen.library)
	if err != nil || len(again) != 1 || again[0].PeerName() != "dana" || !again[0].Encrypted() || again[0].User != "hen" {
		t.Errorf("hen's copies at start: %v %v", again, err)
	}

	// hen's second swap needs the same passphrase.
	inv2 := gqlData[invite](t, dana.graphql(t, annPhone, `mutation { swapInvite(name: "hen", quota: 1000) { link } }`, nil), "swapInvite")
	if r := join(map[string]any{"link": inv2.Link, "name": "ann", "passphrase": "another long passphrase of six words"}); r.Reason != "WRONG_PASSPHRASE" {
		t.Errorf("another passphrase: %+v", r)
	}

	// Removed from hen's phone: no more copies go there. Revoked from dana's: hen's server is refused, files deleted.
	if resp := hen.graphql(t, henPhone, `mutation { removeSwap(name: "dana") }`, nil); len(resp.Errors) > 0 {
		t.Fatal(resp.Errors)
	}
	if hen.swaps.copierOn("hen", peerKey("hen", "dana")) != nil || len(hen.swaps.Mine("hen")) != 0 {
		t.Error("the swap is still there after removeSwap")
	}
	if resp := dana.graphql(t, danaPhone, `mutation { revokeSwap(name: "hen", deleteFiles: true) }`, nil); len(resp.Errors) > 0 {
		t.Fatal(resp.Errors)
	}
	if _, err := os.Stat(filepath.Join(dana.config.PeersFolder, "dana", "hen")); !os.IsNotExist(err) {
		t.Errorf("hen's files at dana's: %v", err)
	}
	if ds := gqlData[swaps](t, dana.graphql(t, danaPhone, swapsQuery, nil), "swaps"); len(ds.Kept) != 0 {
		t.Errorf("dana still keeps: %+v", ds.Kept)
	}
}

// A plain copy, for a friend the user trusts, needs no passphrase; a command-line peer can't take a user's name.
func TestAPlainSwapAndNames(t *testing.T) {
	dana := swapEnv(t, "dana")
	hen := swapEnv(t, "hen", "ann")
	henPhone := hen.enrolledClient(t)
	link := gqlData[struct{ Link string }](t, dana.graphql(t, dana.enrolledClientOf(t, "dana"),
		`mutation { swapInvite(name: "hen", quota: 1000000) { link } }`, nil), "swapInvite").Link
	r := gqlData[joinResult](t, hen.graphql(t, henPhone, joinSwapMutation, map[string]any{"link": link, "name": "dana", "plain": true}), "joinSwap")
	if r.Typename != "SwapJoined" || r.Swap.Encrypted {
		t.Fatalf("plain: %+v", r)
	}
	// The code works once: ann, on the same server, can't use it again.
	annPhone := hen.enrolledClientOf(t, "ann")
	if r := gqlData[joinResult](t, hen.graphql(t, annPhone, joinSwapMutation, map[string]any{"link": link, "name": "dana", "plain": true}), "joinSwap"); r.Reason != "CODE_REFUSED" {
		t.Errorf("the code worked twice: %+v", r)
	}
	hen.uploadPhoto(t, henPhone, []byte("plain"), "IMG_1.jpg")
	runCopy(context.Background(), hen.swaps.copierOn("hen", peerKey("hen", "dana")))
	found := false
	filepath.WalkDir(filepath.Join(dana.config.PeersFolder, "dana", "hen", "hen", "media"), func(_ string, d os.DirEntry, err error) error {
		found = found || (err == nil && d.Name() == "IMG_1.jpg")
		return nil
	})
	if !found {
		t.Error("the plain copy isn't at dana's")
	}
	if _, _, err := dana.peers.Invite("", "dana", 1000); err == nil {
		t.Error("a command-line peer took a user's name")
	}
}
