package peers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/journal/server-go/enroll"
	"github.com/journal/server-go/media"
)

// partialDir holds resumed uploads in a user's space, by their sha256; a listing never shows it.
const partialDir = ".journal-partial"

// diskReserve stays free on the host's disk whatever the quotas say.
const diskReserve = 1 << 30

// Host keeps the peers' files in Folder/<peer>/<user>/.
type Host struct {
	Folder   string
	Registry *Registry
	issuer   *enroll.Issuer
	codes    *enroll.CodeStore
	limiter  *enroll.Limiter
	now      func() time.Time

	mu      sync.Mutex
	invites map[string]int64 // quota by name, for the codes pending
	used    map[string]int64 // bytes by peer, counted at the first ask
	putting map[string]bool  // uploads under way, by partial file
}

// NewHost serves the peers in registry from folder; issuer signs their certificates.
func NewHost(folder string, registry *Registry, issuer *enroll.Issuer, now func() time.Time) *Host {
	return &Host{Folder: folder, Registry: registry, issuer: issuer, codes: enroll.NewCodeStore(now),
		limiter: enroll.NewLimiter(now), now: now, invites: map[string]int64{}, used: map[string]int64{},
		putting: map[string]bool{}}
}

// ValidName is a peer's or a user's name: one folder, the same on every platform.
func ValidName(name string) bool {
	if name == "" || len(name) > 64 || strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\:*?"<>|`) {
		return false
	}
	return filepath.IsLocal(name)
}

// Invite makes a one-time code for a peer named name with quota bytes, valid enroll.CodeTTL. A revoked peer's name
// may be given again: the new peer then has the old one's files (a server set up again after it was lost).
func (h *Host) Invite(name string, quota int64) (string, time.Time, error) {
	if !ValidName(name) {
		return "", time.Time{}, fmt.Errorf("%q can't name a peer: letters, digits and dashes", name)
	}
	if quota <= 0 {
		return "", time.Time{}, errors.New("a peer needs a quota")
	}
	if _, ok := h.Registry.Active(name); ok {
		return "", time.Time{}, fmt.Errorf("peer %s is already added; revoke it first to invite it again", name)
	}
	h.mu.Lock()
	h.invites[name] = quota
	h.mu.Unlock()
	code, expires := h.codes.Create(name)
	return code, expires, nil
}

// ServeEnroll trades an invite code and a certificate request for the peer's certificate (POST /peer/enroll).
func (h *Host) ServeEnroll(w http.ResponseWriter, r *http.Request) {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if !h.limiter.Allow(ip) {
		answerError(w, http.StatusTooManyRequests, CodeRateLimited, "too many wrong codes; try again in a minute")
		return
	}
	var req EnrollRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		answerError(w, http.StatusBadRequest, CodeInvalidCSR, "not an enroll request")
		return
	}
	csr, err := enroll.ParseDeviceCSR(req.CSR)
	if err != nil {
		answerError(w, http.StatusBadRequest, CodeInvalidCSR, err.Error())
		return
	}
	pending, ok := h.codes.Redeem(req.Code)
	if !ok {
		h.limiter.Fail(ip)
		answerError(w, http.StatusForbidden, CodeInvalidCode, "the code is wrong, used or expired")
		return
	}
	name := pending.User
	h.mu.Lock()
	quota := h.invites[name]
	delete(h.invites, name)
	h.mu.Unlock()
	cert, err := h.issuer.IssuePeer(csr, name)
	if err != nil {
		answerError(w, http.StatusInternalServerError, "", err.Error())
		return
	}
	peer := Peer{Name: name, Serial: enroll.SerialString(cert.SerialNumber), Quota: quota, AddedAt: h.now().UTC(),
		ExpiresAt: cert.NotAfter.UTC()}
	if err := h.Registry.add(peer); err != nil {
		answerError(w, http.StatusInternalServerError, "", err.Error())
		return
	}
	log.Printf("Peer %s added from %s, quota %d bytes", name, r.RemoteAddr, quota)
	writeJSON(w, Enrolled{Name: name, Quota: quota, Certificate: cert.Raw})
}

// peerOf is the request's peer: a peer certificate (enroll.PeerUnit) the registry knows and hasn't revoked.
func (h *Host) peerOf(r *http.Request) (Peer, bool) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return Peer{}, false
	}
	cert := r.TLS.PeerCertificates[0]
	if !enroll.IsPeer(cert) {
		return Peer{}, false
	}
	serial := enroll.SerialString(cert.SerialNumber)
	p, ok := h.Registry.BySerial(serial)
	if !ok || p.RevokedAt != nil || p.Name != cert.Subject.CommonName {
		return Peer{}, false
	}
	if p.PreviousSerial != "" && serial == p.Serial {
		h.Registry.used(serial)
		log.Printf("Peer %s uses its renewed certificate: the one before is refused from now on", p.Name)
	}
	return p, true
}

// ServeRenew answers POST /peer/renew: a new certificate for the peer, for the request's key ({csr}), the same name
// and quota. The one it called with works until the new one is first used.
func ServeRenew(h *Host, p Peer, w http.ResponseWriter, r *http.Request) {
	var req EnrollRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		answerError(w, http.StatusBadRequest, CodeInvalidCSR, "not a renew request")
		return
	}
	csr, err := enroll.ParseDeviceCSR(req.CSR)
	if err != nil {
		answerError(w, http.StatusBadRequest, CodeInvalidCSR, err.Error())
		return
	}
	cert, err := h.issuer.IssuePeer(csr, p.Name)
	if err != nil {
		answerError(w, http.StatusInternalServerError, "", err.Error())
		return
	}
	if err := h.Registry.renewed(p.Name, enroll.SerialString(cert.SerialNumber), cert.NotAfter.UTC()); err != nil {
		answerError(w, http.StatusInternalServerError, "", err.Error())
		return
	}
	log.Printf("Peer %s renewed its certificate, until %s", p.Name, cert.NotAfter.Format("2006-01-02"))
	writeJSON(w, Enrolled{Name: p.Name, Quota: p.Quota, Certificate: cert.Raw})
}

// Handler is the peer API but enroll, for the routes under /peer/ (serve.go names them).
func (h *Host) Handler(serve func(h *Host, p Peer, w http.ResponseWriter, r *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.peerOf(r)
		if !ok {
			log.Printf("Peer API refused from %s (%s %s): no peer certificate, or revoked", r.RemoteAddr, r.Method, r.URL.Path)
			http.Error(w, "peer certificate required", http.StatusUnauthorized)
			return
		}
		serve(h, p, w, r)
	})
}

// space is a peer's folder.
func (h *Host) space(p Peer) string { return filepath.Join(h.Folder, p.Name) }

// local is a user's path in the peer's space, or "" for one that isn't safe.
func (h *Host) local(p Peer, user, rel string) string {
	if !ValidName(user) || rel == "" || strings.Contains(rel, `\`) || path.Clean(rel) != rel {
		return ""
	}
	if first, _, _ := strings.Cut(rel, "/"); first == partialDir {
		return ""
	}
	local, err := filepath.Localize(rel)
	if err != nil {
		return ""
	}
	return filepath.Join(h.space(p), user, local)
}

// Used is the bytes a peer's files take, counted once and then kept up as they change.
func (h *Host) Used(p Peer) int64 {
	h.mu.Lock()
	used, ok := h.used[p.Name]
	h.mu.Unlock()
	if ok {
		return used
	}
	var total int64
	filepath.WalkDir(h.space(p), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && d.Name() == partialDir {
			return filepath.SkipDir // counted once in place
		}
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	if used, ok := h.used[p.Name]; ok {
		return used
	}
	h.used[p.Name] = total
	return total
}

func (h *Host) addUsed(p Peer, n int64) {
	h.Used(p)
	h.mu.Lock()
	h.used[p.Name] += n
	h.mu.Unlock()
}

// SpaceOf is how much p may still store.
func (h *Host) SpaceOf(p Peer) Space {
	s := Space{Quota: p.Quota, Used: h.Used(p)}
	s.Free = max(0, s.Quota-s.Used)
	os.MkdirAll(h.space(p), 0o755)
	if disk, err := media.FreeBytes(h.space(p)); err == nil {
		s.Free = min(s.Free, max(0, int64(disk)-diskReserve))
	}
	return s
}

// ServeSpace answers GET /peer/space.
func ServeSpace(h *Host, p Peer, w http.ResponseWriter, r *http.Request) { writeJSON(w, h.SpaceOf(p)) }

// ServeList answers GET /peer/{user}/list: the files under prefix (a folder ending in "/", or "" for the root),
// in no set order, one ListEntry per line; only the folder's own files unless recursive=1. Dot names are left out.
func ServeList(h *Host, p Peer, w http.ResponseWriter, r *http.Request) {
	user, prefix := r.PathValue("user"), r.URL.Query().Get("prefix")
	recursive := r.URL.Query().Get("recursive") == "1"
	var root string
	switch {
	case prefix == "":
		if ValidName(user) {
			root = filepath.Join(h.space(p), user)
		}
	case strings.HasSuffix(prefix, "/"):
		root = h.local(p, user, strings.TrimSuffix(prefix, "/"))
	}
	if root == "" {
		http.Error(w, "bad prefix", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := json.NewEncoder(w)
	err := filepath.WalkDir(root, func(file string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if strings.HasPrefix(d.Name(), ".") && file != root {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if file != root && !recursive {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // gone meanwhile
		}
		rel, _ := filepath.Rel(root, file)
		return enc.Encode(ListEntry{Path: prefix + filepath.ToSlash(rel), Size: info.Size(), Modified: info.ModTime()})
	})
	if err != nil {
		log.Printf("Peer %s: listing %s/%s: %v", p.Name, user, prefix, err)
	}
}

// ServeFile answers HEAD, GET, PUT, PATCH and DELETE of /peer/{user}/files/{path}.
func ServeFile(h *Host, p Peer, w http.ResponseWriter, r *http.Request) {
	user, rel := r.PathValue("user"), r.PathValue("path")
	file := h.local(p, user, rel)
	if file == "" {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		f, err := os.Open(file)
		if err != nil {
			fileError(w, err)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set(HeaderModified, info.ModTime().Format(time.RFC3339Nano))
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", info.ModTime(), f)
	case http.MethodPut:
		h.put(p, user, file, w, r)
	case http.MethodPatch:
		modified, err := time.Parse(time.RFC3339Nano, r.Header.Get(HeaderModified))
		if err != nil {
			http.Error(w, "no "+HeaderModified, http.StatusBadRequest)
			return
		}
		if err := os.Chtimes(file, modified, modified); err != nil {
			fileError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		info, err := os.Stat(file)
		if err == nil {
			err = os.Remove(file)
		}
		if err != nil {
			fileError(w, err)
			return
		}
		h.addUsed(p, -info.Size())
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT, PATCH, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// put writes a file whole or not at all. Without Content-Range the body is the whole file, through a temp file. With
// "Content-Range: bytes start-end/total" it adds to the upload kept by its sha256 (HEAD …/upload/{sha256} says how
// much is here), and the file takes its place once all total bytes are in and match HeaderSha256.
func (h *Host) put(p Peer, user, file string, w http.ResponseWriter, r *http.Request) {
	sum := strings.ToLower(r.Header.Get(HeaderSha256))
	modified, err := time.Parse(time.RFC3339Nano, r.Header.Get(HeaderModified))
	if !validSha(sum) || err != nil {
		http.Error(w, "needs "+HeaderSha256+" and "+HeaderModified, http.StatusBadRequest)
		return
	}
	start, total := int64(0), r.ContentLength
	resumed := r.Header.Get("Content-Range") != ""
	if resumed {
		var end int64
		if _, err := fmt.Sscanf(r.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err != nil ||
			start < 0 || end < start || end >= total {
			http.Error(w, "bad Content-Range", http.StatusBadRequest)
			return
		}
	}
	if total < 0 {
		http.Error(w, "needs Content-Length", http.StatusLengthRequired)
		return
	}
	var existing int64
	if info, err := os.Stat(file); err == nil {
		existing = info.Size()
	}
	if s := h.SpaceOf(p); total-existing > s.Free {
		answerError(w, http.StatusInsufficientStorage, CodeQuotaFull,
			fmt.Sprintf("%d bytes needed, %d free of the quota", total-existing, s.Free))
		return
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		fileError(w, err)
		return
	}

	var tmp string
	if resumed {
		dir := filepath.Join(h.space(p), user, partialDir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fileError(w, err)
			return
		}
		tmp = filepath.Join(dir, sum)
		h.mu.Lock()
		busy := h.putting[tmp]
		h.putting[tmp] = true
		h.mu.Unlock()
		if busy {
			http.Error(w, "this file is being put already", http.StatusConflict)
			return
		}
		defer func() { h.mu.Lock(); delete(h.putting, tmp); h.mu.Unlock() }()
		var have int64
		if info, err := os.Stat(tmp); err == nil {
			have = info.Size()
		}
		if have != start {
			w.Header().Set(HeaderOffset, strconv.FormatInt(have, 10))
			answerError(w, http.StatusConflict, CodeWrongOffset, fmt.Sprintf("the upload has %d bytes here", have))
			return
		}
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			fileError(w, err)
			return
		}
		_, err = io.Copy(f, io.LimitReader(r.Body, total-start))
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			log.Printf("Peer %s: putting %s/%s stopped: %v", p.Name, user, r.PathValue("path"), err)
			answerError(w, http.StatusBadRequest, "", "the upload stopped; what arrived is kept") // for the next try
			return
		}
		info, err := os.Stat(tmp)
		if err != nil {
			fileError(w, err)
			return
		}
		if info.Size() < total {
			w.Header().Set(HeaderOffset, strconv.FormatInt(info.Size(), 10))
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if got, _, err := hashFile(tmp); err != nil || got != sum || info.Size() != total {
			os.Remove(tmp)
			answerError(w, http.StatusUnprocessableEntity, CodeDamaged, "the bytes aren't the ones named; send the file again")
			return
		}
	} else {
		f, err := os.CreateTemp(filepath.Dir(file), "."+filepath.Base(file)+".*.putting")
		if err != nil {
			fileError(w, err)
			return
		}
		tmp = f.Name()
		defer os.Remove(tmp)
		hash := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(r.Body, total))
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			log.Printf("Peer %s: putting %s/%s stopped: %v", p.Name, user, r.PathValue("path"), err)
			answerError(w, http.StatusBadRequest, "", "the upload stopped")
			return
		}
		if hex.EncodeToString(hash.Sum(nil)) != sum || n != total {
			answerError(w, http.StatusUnprocessableEntity, CodeDamaged, "the bytes aren't the ones named")
			return
		}
	}
	if err := media.Rename(tmp, file); err != nil {
		fileError(w, err)
		return
	}
	os.Chtimes(file, modified, modified)
	h.addUsed(p, total-existing)
	w.WriteHeader(http.StatusCreated)
}

// ServeUpload answers HEAD /peer/{user}/upload/{sha256}: HeaderOffset is how much of that upload is here.
func ServeUpload(h *Host, p Peer, w http.ResponseWriter, r *http.Request) {
	user, sum := r.PathValue("user"), strings.ToLower(r.PathValue("sha256"))
	if !ValidName(user) || !validSha(sum) {
		http.Error(w, "bad upload", http.StatusBadRequest)
		return
	}
	var have int64
	if info, err := os.Stat(filepath.Join(h.space(p), user, partialDir, sum)); err == nil {
		have = info.Size()
	}
	w.Header().Set(HeaderOffset, strconv.FormatInt(have, 10))
	w.WriteHeader(http.StatusOK)
}

// ServeMove answers POST /peer/{user}/move/{path}: the file goes to HeaderMoveTo in the same space, where no file
// may be. The space's used bytes don't change.
func ServeMove(h *Host, p Peer, w http.ResponseWriter, r *http.Request) {
	user := r.PathValue("user")
	from, to := h.local(p, user, r.PathValue("path")), h.local(p, user, r.Header.Get(HeaderMoveTo))
	if from == "" || to == "" {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(from); err != nil {
		fileError(w, err)
		return
	}
	if _, err := os.Stat(to); err == nil {
		http.Error(w, "a file is there already", http.StatusConflict)
		return
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		fileError(w, err)
		return
	}
	if err := os.Rename(from, to); err != nil {
		fileError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ServeSha256 answers GET /peer/{user}/sha256/{path}: the file's sha256 and size, hashed here so the peer needn't
// download it.
func ServeSha256(h *Host, p Peer, w http.ResponseWriter, r *http.Request) {
	file := h.local(p, r.PathValue("user"), r.PathValue("path"))
	if file == "" {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	sum, size, err := hashFile(file)
	if err != nil {
		fileError(w, err)
		return
	}
	writeJSON(w, map[string]any{"sha256": sum, "size": size})
}

func hashFile(file string) (string, int64, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, f)
	return hex.EncodeToString(hash.Sum(nil)), n, err
}

func validSha(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func fileError(w http.ResponseWriter, err error) {
	if errors.Is(err, fs.ErrNotExist) {
		http.Error(w, "no such file", http.StatusNotFound)
		return
	}
	log.Printf("Peer API: %v", err)
	http.Error(w, "the host couldn't do it", http.StatusInternalServerError)
}

func answerError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// Mount adds the peer API to mux (the server's, beside the devices' routes).
func (h *Host) Mount(mux *http.ServeMux) {
	mux.Handle("POST /peer/enroll", http.HandlerFunc(h.ServeEnroll))
	mux.Handle("POST /peer/renew", h.Handler(ServeRenew))
	mux.Handle("GET /peer/space", h.Handler(ServeSpace))
	mux.Handle("GET /peer/{user}/list", h.Handler(ServeList))
	mux.Handle("/peer/{user}/files/{path...}", h.Handler(ServeFile))
	mux.Handle("GET /peer/{user}/sha256/{path...}", h.Handler(ServeSha256))
	mux.Handle("HEAD /peer/{user}/upload/{sha256}", h.Handler(ServeUpload))
	mux.Handle("POST /peer/{user}/move/{path...}", h.Handler(ServeMove))
}
