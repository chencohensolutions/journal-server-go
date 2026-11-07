package copies

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/journal/server-go/peers"
)

// errRefused is a host that refuses this server's certificate (revoked): out of reach, and not worth trying again.
var errRefused = errors.New("the host refused this server's certificate (revoked?)")

// ErrQuotaFull stops a pass when the peer has no room left of the quota its host gave this server.
var ErrQuotaFull = errors.New("the peer's quota is full")

// Peer is a copy on a friend's journal server (docs/peers.md): this server pushes files to the host's peer API, in
// User's space there. It lists each year of the copy once and keeps the listing, kept up as it puts and removes, so
// a pass doesn't ask the host about every file: only this server writes there.
type Peer struct {
	Name string // as `journal-server peer add` named it, or the user who joined a swap
	// Its folder in peers_dir, relative: Name, or <user>/<name> for a user's own peer (a swap).
	Key  string
	URL  string // the host, "https://dana.example:56398"
	User string
	// Most bytes a second to send, 0 for no limit.
	BytesPerSecond int64

	client *http.Client
	cert   *certHolder
	dialer *dialer

	mu      sync.Mutex
	listed  map[string]listing // by group (listGroup)
	space   *peers.Space
	spaceAt time.Time
}

type listing struct {
	at    time.Time
	files map[string]File // by lower-case path
}

// listFor is how long a listing is trusted; another is fetched after it, in case the host's admin changed files.
var listFor = 30 * time.Minute

// spaceFor is how long the host's answer about space is trusted, less what was put since.
const spaceFor = time.Minute

// NewPeer reaches the host at url with the peer certificate cert, trusting only a server whose key has fingerprint
// (peers.Fingerprint), as the invite named it.
func NewPeer(name, url, user string, cert tls.Certificate, fingerprint string) *Peer {
	holder := &certHolder{}
	holder.Store(&cert)
	d := &dialer{net: net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}}
	return &Peer{Name: name, Key: name, URL: strings.TrimSuffix(url, "/"), User: user, client: peerClient(holder, fingerprint, d),
		cert: holder, dialer: d, listed: map[string]listing{}}
}

// SetAddressBook lets the host be reached at the addresses book knows, not only at URL's (a home line whose IP
// changes). Connections already open stay.
func (p *Peer) SetAddressBook(book AddressBook) { p.dialer.book.Store(&book) }

// certHolder is the certificate a peer client shows, swapped when it is renewed.
type certHolder struct {
	atomic.Pointer[tls.Certificate]
}

// Certificate is the peer certificate this server shows the host.
func (p *Peer) Certificate() tls.Certificate { return *p.cert.Load() }

// SetCertificate shows cert from the next connection on (a renewed certificate).
func (p *Peer) SetCertificate(cert tls.Certificate) {
	p.cert.Store(&cert)
	p.client.CloseIdleConnections()
}

// Renew asks the host for a new certificate for the key in csr (POST /peer/renew); its DER.
func (p *Peer) Renew(csr []byte) ([]byte, error) {
	body, _ := json.Marshal(peers.EnrollRequest{CSR: csr})
	req, err := http.NewRequest(http.MethodPost, p.URL+"/peer/renew", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	resp, err := p.do(req, http.StatusOK)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var renewed peers.Enrolled
	if err := json.NewDecoder(resp.Body).Decode(&renewed); err != nil {
		return nil, err
	}
	return renewed.Certificate, nil
}

// PeerClient is an HTTPS client that shows cert and accepts only the server key fingerprint names; it dials the
// addresses book knows when book isn't nil.
func PeerClient(cert tls.Certificate, fingerprint string, book AddressBook) *http.Client {
	holder := &certHolder{}
	holder.Store(&cert)
	d := &dialer{net: net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}}
	if book != nil {
		d.book.Store(&book)
	}
	return peerClient(holder, fingerprint, d)
}

func peerClient(cert *certHolder, fingerprint string, d *dialer) *http.Client {
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		// The host's certificate is signed by its own authority, which this server doesn't know: the pinned key is
		// what is trusted, checked below.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the host showed no certificate")
			}
			if got := peers.Fingerprint(cs.PeerCertificates[0]); got != fingerprint {
				return fmt.Errorf("the host's key has fingerprint %s, not the %s it was added with", got, fingerprint)
			}
			return nil
		},
	}
	tlsConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		return cert.Load(), nil // an empty one shows none (peer add)
	}
	return &http.Client{Transport: &http.Transport{
		DialContext:     d.DialContext,
		TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true, ResponseHeaderTimeout: time.Minute,
		IdleConnTimeout: 2 * time.Minute, TLSHandshakeTimeout: 30 * time.Second,
	}}
}

func (p *Peer) url(kind, rel string) string {
	escaped := strings.Split(rel, "/")
	for i, part := range escaped {
		escaped[i] = url.PathEscape(part)
	}
	return p.URL + "/peer/" + url.PathEscape(p.User) + "/" + kind + "/" + strings.Join(escaped, "/")
}

// do sends a request with a deadline for small answers; ok lists the statuses that aren't errors.
func (p *Peer) do(req *http.Request, ok ...int) (*http.Response, error) {
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	for _, status := range ok {
		if resp.StatusCode == status {
			return resp, nil
		}
	}
	defer resp.Body.Close()
	return nil, answerError(resp)
}

// answerError is what a host's error answer means.
func answerError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var answer struct{ Error, Message string }
	json.Unmarshal(body, &answer)
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fs.ErrNotExist
	case answer.Error == peers.CodeQuotaFull:
		return fmt.Errorf("%w: %s", ErrQuotaFull, answer.Message)
	case answer.Error == peers.CodeDamaged:
		return fmt.Errorf("%w: %s", errDamaged, answer.Message)
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("%w: %w", ErrUnreachable, errRefused)
	case answer.Message != "":
		return fmt.Errorf("the host answered %s: %s", resp.Status, answer.Message)
	}
	return fmt.Errorf("the host answered %s: %s", resp.Status, strings.TrimSpace(string(body)))
}

func (p *Peer) request(method, url string, body io.Reader) (*http.Request, error) {
	return http.NewRequest(method, url, body)
}

// Ready asks the host now (a pass or a batch starts with it).
func (p *Peer) Ready() error {
	_, err := p.askSpace()
	return err
}

// Space is the host's answer about this server's space, asked again after spaceFor.
func (p *Peer) Space() (peers.Space, error) {
	p.mu.Lock()
	if p.space != nil && time.Since(p.spaceAt) < spaceFor {
		s := *p.space
		p.mu.Unlock()
		return s, nil
	}
	p.mu.Unlock()
	return p.askSpace()
}

func (p *Peer) askSpace() (peers.Space, error) {
	req, err := p.request(http.MethodGet, p.URL+"/peer/space", nil)
	if err != nil {
		return peers.Space{}, err
	}
	resp, err := p.do(req, http.StatusOK)
	if err != nil {
		return peers.Space{}, err
	}
	defer resp.Body.Close()
	var s peers.Space
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return peers.Space{}, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	p.mu.Lock()
	p.space, p.spaceAt = &s, time.Now()
	p.mu.Unlock()
	return s, nil
}

func (p *Peer) Free(string) (uint64, error) {
	s, err := p.Space()
	return uint64(max(0, s.Free)), err
}

// listGroup is the listing a path's file is in: a year of media/ (listed whole), or the path's own folder.
func listGroup(rel string) (prefix string, recursive bool) {
	parts := strings.Split(rel, "/")
	if len(parts) >= 3 && parts[0] == "media" {
		return "media/" + parts[1] + "/", true
	}
	dir := path.Dir(rel)
	if dir == "." {
		return "", false
	}
	return dir + "/", false
}

// group is the listing of prefix, fetched when there is none or it is old.
func (p *Peer) group(prefix string, recursive bool) (listing, error) {
	key := prefix
	if !recursive {
		key += "\x00"
	}
	p.mu.Lock()
	l, ok := p.listed[key]
	p.mu.Unlock()
	if ok && time.Since(l.at) < listFor {
		return l, nil
	}
	l = listing{at: time.Now(), files: map[string]File{}}
	err := p.list(prefix, recursive, func(f File) error {
		l.files[strings.ToLower(f.Path)] = f
		return nil
	})
	if err != nil {
		return listing{}, err
	}
	p.mu.Lock()
	p.listed[key] = l
	p.mu.Unlock()
	return l, nil
}

func (p *Peer) list(prefix string, recursive bool, fn func(File) error) error {
	q := url.Values{"prefix": {prefix}}
	if recursive {
		q.Set("recursive", "1")
	}
	req, err := p.request(http.MethodGet, p.URL+"/peer/"+url.PathEscape(p.User)+"/list?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := p.do(req, http.StatusOK)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var e peers.ListEntry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			return fmt.Errorf("%w: a listing line: %w", ErrUnreachable, err)
		}
		if err := fn(File{Path: e.Path, Size: e.Size, ModTime: e.Modified}); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	return nil
}

// changed keeps the listings that hold rel up to date: f, or gone when f is nil.
func (p *Peer) changed(rel string, f *File) {
	p.mu.Lock()
	defer p.mu.Unlock()
	prefix, recursive := listGroup(rel)
	keys := []string{prefix}
	if !recursive {
		keys[0] += "\x00"
	}
	if dir := path.Dir(rel); dir != "." {
		keys = append(keys, dir+"/\x00")
	}
	for _, key := range keys {
		l, ok := p.listed[key]
		if !ok {
			continue
		}
		if f == nil {
			delete(l.files, strings.ToLower(rel))
		} else {
			l.files[strings.ToLower(rel)] = *f
		}
	}
}

func (p *Peer) Stat(rel string) (File, error) {
	l, err := p.group(listGroup(rel))
	if err != nil {
		return File{}, err
	}
	p.mu.Lock()
	f, ok := l.files[strings.ToLower(rel)]
	p.mu.Unlock()
	if !ok {
		return File{}, fmt.Errorf("%s: %w", rel, fs.ErrNotExist)
	}
	return f, nil
}

func (p *Peer) List(dir string) ([]File, error) {
	// A month of media/ is in its year's listing.
	prefix, recursive := listGroup(dir + "/x")
	if !recursive {
		prefix = dir + "/"
	}
	l, err := p.group(prefix, recursive)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var files []File
	for _, f := range l.files {
		if path.Dir(f.Path) == dir {
			files = append(files, f)
		}
	}
	return files, nil
}

// Walk lists dir whole from the host (and keeps the years of media/ it lists).
func (p *Peer) Walk(dir string, fn func(File) error) error {
	years := map[string]map[string]File{}
	err := p.list(dir+"/", true, func(f File) error {
		if prefix, recursive := listGroup(f.Path); recursive {
			if years[prefix] == nil {
				years[prefix] = map[string]File{}
			}
			years[prefix][strings.ToLower(f.Path)] = f
		}
		return fn(f)
	})
	if err == nil && dir == "media" {
		p.mu.Lock()
		for prefix, files := range years {
			p.listed[prefix] = listing{at: time.Now(), files: files}
		}
		p.mu.Unlock()
	}
	return err
}

func (p *Peer) Open(rel string) (io.ReadCloser, error) {
	req, err := http.NewRequest(http.MethodGet, p.url("files", rel), nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.do(req, http.StatusOK)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Put sends the file in one request, or what the host still lacks of it: a large file is resumed where an earlier
// try stopped (the host keeps what arrived by its sha256). r is read from its start either way.
func (p *Peer) Put(rel string, r io.Reader, size int64, sum string, modTime time.Time) error {
	read := &counting{r: r}
	r = p.limit(read)
	var req *http.Request
	var err error
	if size < resumeFrom {
		req, err = http.NewRequest(http.MethodPut, p.url("files", rel), r)
		if err != nil {
			return err
		}
		req.ContentLength = size
	} else {
		offset, err := p.offset(sum)
		if err != nil {
			return err
		}
		if offset >= size {
			offset = 0 // a whole upload left behind (or not this file's): send it again, the host checks it
		}
		if _, err := io.CopyN(io.Discard, r, offset); err != nil {
			return err
		}
		req, err = http.NewRequest(http.MethodPut, p.url("files", rel), r)
		if err != nil {
			return err
		}
		req.ContentLength = size - offset
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, max(offset, size-1), size))
	}
	req.Header.Set(peers.HeaderSha256, sum)
	req.Header.Set(peers.HeaderModified, modTime.Format(time.RFC3339Nano))
	resp, err := p.do(req, http.StatusCreated)
	if read.err != nil {
		return read.err // the file couldn't be read, or the pass was stopped: not the network's doing
	}
	if read.n < size {
		return fmt.Errorf("%w: the file ended after %d of %d bytes", errDamaged, read.n, size)
	}
	if err != nil {
		return err
	}
	resp.Body.Close()
	p.changed(rel, &File{Path: rel, Size: size, ModTime: modTime})
	p.mu.Lock()
	if p.space != nil {
		p.space.Free -= size
		p.space.Used += size
	}
	p.mu.Unlock()
	return nil
}

// resumeFrom is the size from which a put can be resumed; smaller files are sent again whole.
var resumeFrom int64 = 4 << 20

func (p *Peer) offset(sum string) (int64, error) {
	req, err := p.request(http.MethodHead, p.URL+"/peer/"+url.PathEscape(p.User)+"/upload/"+sum, nil)
	if err != nil {
		return 0, err
	}
	resp, err := p.do(req, http.StatusOK)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return strconv.ParseInt(resp.Header.Get(peers.HeaderOffset), 10, 64)
}

func (p *Peer) Touch(rel string, modTime time.Time) error {
	req, err := p.request(http.MethodPatch, p.url("files", rel), nil)
	if err != nil {
		return err
	}
	req.Header.Set(peers.HeaderModified, modTime.Format(time.RFC3339Nano))
	resp, err := p.do(req, http.StatusNoContent)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if f, err := p.Stat(rel); err == nil {
		f.ModTime = modTime
		p.changed(rel, &f)
	}
	return nil
}

func (p *Peer) Remove(rel string) error {
	req, err := p.request(http.MethodDelete, p.url("files", rel), nil)
	if err != nil {
		return err
	}
	resp, err := p.do(req, http.StatusNoContent)
	if err != nil {
		return err
	}
	resp.Body.Close()
	p.changed(rel, nil)
	return nil
}

func (p *Peer) Move(from, to string) error {
	req, err := p.request(http.MethodPost, p.url("move", from), nil)
	if err != nil {
		return err
	}
	req.Header.Set(peers.HeaderMoveTo, to)
	resp, err := p.do(req, http.StatusNoContent)
	if err != nil {
		return err
	}
	resp.Body.Close()
	f, statErr := p.Stat(from)
	p.changed(from, nil)
	if statErr == nil {
		f.Path = to
		p.changed(to, &f)
	}
	return nil
}

// Sha256 is the sha256 and size of a file in the copy, hashed by the host (nothing is downloaded).
func (p *Peer) Sha256(rel string) (string, int64, error) {
	req, err := p.request(http.MethodGet, p.url("sha256", rel), nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := p.do(req, http.StatusOK)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	var answer struct {
		Sha256 string `json:"sha256"`
		Size   int64  `json:"size"`
	}
	err = json.NewDecoder(resp.Body).Decode(&answer)
	return answer.Sha256, answer.Size, err
}

// counting is a source whose reads are counted, and its first error other than EOF kept.
type counting struct {
	r   io.Reader
	n   int64
	err error
}

func (c *counting) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	if err != nil && err != io.EOF && c.err == nil {
		c.err = err
	}
	return n, err
}

// limit paces r to BytesPerSecond.
func (p *Peer) limit(r io.Reader) io.Reader {
	if p.BytesPerSecond <= 0 {
		return r
	}
	return &paced{r: r, rate: p.BytesPerSecond, start: time.Now()}
}

type paced struct {
	r     io.Reader
	rate  int64
	start time.Time
	n     int64
}

func (p *paced) Read(b []byte) (int, error) {
	if len(b) > int(p.rate/4)+1 {
		b = b[:p.rate/4+1] // a quarter second at a time
	}
	n, err := p.r.Read(b)
	p.n += int64(n)
	if ahead := time.Duration(float64(p.n)/float64(p.rate)*float64(time.Second)) - time.Since(p.start); ahead > 0 {
		time.Sleep(ahead)
	}
	return n, err
}
