package discovery

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/dht/v2"
)

func TestServerIDs(t *testing.T) {
	sum := sha256.Sum256([]byte("a server key"))
	fingerprint := hex.EncodeToString(sum[:])
	id := FormatID(fingerprint)
	if len(id) != 63 || strings.Count(id, "-") != 7 {
		t.Fatalf("id %q", id)
	}
	for _, typed := range []string{id, strings.ToLower(id), strings.ReplaceAll(id, "-", " "), strings.ReplaceAll(id, "-", ""), fingerprint, strings.ToUpper(fingerprint)} {
		if got, err := ParseID(typed); err != nil || got != fingerprint {
			t.Errorf("ParseID(%q) = %q, %v", typed, got, err)
		}
	}
	// One letter wrong is found by its group's check letter.
	typo := []byte(id)
	typo[2] = map[bool]byte{true: 'B', false: 'A'}[typo[2] == 'A']
	if _, err := ParseID(string(typo)); err == nil || !strings.Contains(err.Error(), "typo") {
		t.Errorf("a typo: %v", err)
	}
	if _, err := ParseID("ABC"); err == nil {
		t.Error("a short id was read")
	}
}

func TestRecordsAreSealed(t *testing.T) {
	fingerprint := strings.Repeat("ab", 32)
	r := Record{V: 1, Addresses: []string{"https://203.0.113.7:56398", "https://[2001:db8::1]:56398"}, At: time.Now().UTC().Truncate(time.Second)}
	sealed, err := seal(r, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "203.0.113.7") {
		t.Fatal("the address can be read in the sealed record")
	}
	got, err := open(sealed, fingerprint)
	if err != nil || fmt.Sprint(got.Addresses) != fmt.Sprint(r.Addresses) || !got.At.Equal(r.At) {
		t.Fatalf("opened %+v, %v", got, err)
	}
	if _, err := open(sealed, strings.Repeat("cd", 32)); err == nil {
		t.Fatal("opened with another server's fingerprint")
	}

	// Too many addresses for a DHT value: the last are left out.
	var many []string
	for i := range 100 {
		many = append(many, fmt.Sprintf("https://192.168.1.%d:56398", i))
	}
	sealed, err = seal(Record{V: 1, Addresses: many}, fingerprint)
	if err != nil || len(sealed)+4 > maxValue {
		t.Fatalf("%d bytes, %v", len(sealed), err)
	}
	got, _ = open(sealed, fingerprint)
	if len(got.Addresses) == 0 || len(got.Addresses) == 100 || got.Addresses[0] != many[0] {
		t.Fatalf("kept %d addresses", len(got.Addresses))
	}
}

func TestKeysComeFromTheTLSKey(t *testing.T) {
	tlsKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	a, err := KeyFor(tlsKey)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := KeyFor(tlsKey)
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	c, _ := KeyFor(other)
	if !a.Equal(b) || a.Equal(c) {
		t.Fatal("the DHT key isn't one per TLS key")
	}
	if k, err := ParseKey(hex.EncodeToString(a.Public().(ed25519.PublicKey))); err != nil || !k.Equal(a.Public()) {
		t.Fatalf("ParseKey: %v", err)
	}
}

// A small DHT on loopback: one server publishes, another finds its addresses, and a stranger finds nothing.
func TestPublishAndLookUp(t *testing.T) {
	var addrs []dht.Addr
	bootstrap := func() ([]dht.Addr, error) { return addrs, nil }
	var nodes []*Node
	for range 8 {
		conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Skip("no UDP here:", err)
		}
		addrs = append(addrs, dht.NewAddr(conn.LocalAddr()))
		conn.Close() // its port is taken again by the node below
	}
	for _, a := range addrs {
		n := &Node{Bootstrap: bootstrap, Active: true, listen: a.String()}
		t.Cleanup(n.Close)
		nodes = append(nodes, n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// All at once, so each finds the others listening when it joins.
	errs := make(chan error, len(nodes))
	for _, n := range nodes {
		go func() {
			_, err := n.started(ctx)
			errs <- err
		}()
	}
	for range nodes {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}

	tlsKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	key, _ := KeyFor(tlsKey)
	fingerprint := strings.Repeat("12", 32)
	at := time.Now().UTC().Truncate(time.Second)
	if err := nodes[0].Publish(ctx, key, fingerprint, Record{Addresses: []string{"https://203.0.113.7:56398"}, At: at}); err != nil {
		t.Fatal(err)
	}
	got, err := nodes[7].Lookup(ctx, key.Public().(ed25519.PublicKey), fingerprint)
	if err != nil || len(got.Addresses) != 1 || got.Addresses[0] != "https://203.0.113.7:56398" {
		t.Fatalf("found %+v, %v", got, err)
	}
	// Moved: the newer record wins.
	if err := nodes[3].Publish(ctx, key, fingerprint, Record{Addresses: []string{"https://198.51.100.9:56398"}, At: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if got, err := nodes[5].Lookup(ctx, key.Public().(ed25519.PublicKey), fingerprint); err != nil || got.Addresses[0] != "https://198.51.100.9:56398" {
		t.Fatalf("after moving: %+v, %v", got, err)
	}
	stranger, _ := KeyFor(must(ecdsa.GenerateKey(elliptic.P256(), rand.Reader)))
	short, cancelShort := context.WithTimeout(ctx, 10*time.Second)
	defer cancelShort()
	if _, err := nodes[6].Lookup(short, stranger.Public().(ed25519.PublicKey), fingerprint); err != ErrNotFound {
		t.Fatalf("a server that published nothing: %v", err)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestSTUNAnswers(t *testing.T) {
	id := []byte("123456789012")
	answer := make([]byte, 32)
	binary.BigEndian.PutUint16(answer[0:], 0x0101)
	binary.BigEndian.PutUint16(answer[2:], 12)
	binary.BigEndian.PutUint32(answer[4:], stunMagic)
	copy(answer[8:], id)
	binary.BigEndian.PutUint16(answer[20:], 0x0020)
	binary.BigEndian.PutUint16(answer[22:], 8)
	answer[25] = 1
	ip := net.IPv4(203, 0, 113, 7).To4()
	for i := range ip {
		answer[28+i] = ip[i] ^ []byte{0x21, 0x12, 0xA4, 0x42}[i]
	}
	if got := mappedAddress(answer, id); !got.Equal(ip) {
		t.Fatalf("got %v", got)
	}
	if got := mappedAddress(answer, []byte("another id..")); got != nil {
		t.Fatalf("an answer to another request: %v", got)
	}
}

func TestOwnAddresses(t *testing.T) {
	got := Own("https://journal.example:56398/", "56398", net.IPv4(203, 0, 113, 7))
	if len(got) < 2 || got[0] != "https://journal.example:56398" || got[1] != "https://203.0.113.7:56398" {
		t.Fatalf("own: %v", got)
	}
	if hp := HostPorts(append(got, "not a url", "https://[2001:db8::1]:56398/x")); hp[0] != "journal.example:56398" || hp[len(hp)-1] != "[2001:db8::1]:56398" {
		t.Fatalf("host ports: %v", hp)
	}
}
