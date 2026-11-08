// Package update finds, checks and installs a newer journal-server from the project's GitHub Releases
// (docs/install.md, "Updates").
//
// A release (made by .github/workflows/release.yml) holds the program for each system, named by AssetName, and
// SHA256SUMS: its first line "# journal-server <version>", then "<sha256>  <file name>" for every file. SHA256SUMS.sig
// is an ed25519 signature of SHA256SUMS by the release key, whose public half is compiled in (release-keys.txt). A
// release is used only when the signature is good, it names a version newer than this one, and the program
// downloaded has the sha256 SHA256SUMS gives it; the signed version line keeps an old release from being offered
// again under a new tag. GitHub is only where the files are kept: nothing it says is trusted on its own.
package update

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Repo is the GitHub repository whose releases are installed.
const Repo = "chencohensolutions/journal-server-go"

// API is GitHub's API. A build may point it elsewhere (-ldflags -X) to try updates against a local server.
var API = "https://api.github.com"

// Sums and Signature are the names of the checksum file and its signature in every release.
const (
	Sums      = "SHA256SUMS"
	Signature = "SHA256SUMS.sig"
)

//go:embed release-keys.txt
var keysFile string

// ReleaseKeys are the public keys whose signatures are accepted: one base64 ed25519 key per line in
// release-keys.txt (# starts a comment). More than one lets a new key take over from an old one.
func ReleaseKeys() []ed25519.PublicKey {
	keys, _ := ParseKeys(keysFile)
	return keys
}

// ParseKeys reads public keys in the release-keys.txt format.
func ParseKeys(text string) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(line)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("release key %q is not a base64 ed25519 public key", line)
		}
		keys = append(keys, ed25519.PublicKey(raw))
	}
	return keys, nil
}

// AssetName is the release file of the program for a system: journal-server-windows-amd64.exe,
// journal-server-linux-arm64, journal-server-darwin-arm64.
func AssetName(goos, goarch string) string {
	name := "journal-server-" + goos + "-" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// Updater talks to one repository's releases.
type Updater struct {
	// API is GitHub's API ("https://api.github.com"); tests point it elsewhere.
	API    string
	Repo   string
	Keys   []ed25519.PublicKey
	Client *http.Client
	// Current is the running version (version.Number).
	Current string
	// GOOS and GOARCH pick the program; runtime's when empty.
	GOOS, GOARCH string
}

// New is an Updater for this program and the project's releases.
func New(current string) *Updater {
	return &Updater{API: API, Repo: Repo, Keys: ReleaseKeys(),
		Client: &http.Client{Timeout: 30 * time.Minute}, Current: current}
}

// Release is a release that was checked: its signed version and the sha256 of each of its files.
type Release struct {
	Version string
	Tag     string
	URL     string // the release's page
	assets  map[string]string
	sums    map[string]string
}

// Latest finds the newest release, checks its signature and returns it. It doesn't compare versions; see Newer.
func (u *Updater) Latest(ctx context.Context) (*Release, error) {
	if len(u.Keys) == 0 {
		return nil, errors.New("this build has no release key to check releases with")
	}
	var answer struct {
		Tag    string `json:"tag_name"`
		URL    string `json:"html_url"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	body, err := u.get(ctx, u.API+"/repos/"+u.Repo+"/releases/latest", 1<<20)
	if err != nil {
		return nil, fmt.Errorf("the latest release: %w", err)
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("the latest release: %w", err)
	}
	r := &Release{Tag: answer.Tag, URL: answer.URL, assets: map[string]string{}}
	for _, a := range answer.Assets {
		r.assets[a.Name] = a.URL
	}
	if r.assets[Sums] == "" || r.assets[Signature] == "" {
		return nil, fmt.Errorf("release %s has no %s and %s", r.Tag, Sums, Signature)
	}
	sums, err := u.get(ctx, r.assets[Sums], 1<<20)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", Sums, err)
	}
	sig, err := u.get(ctx, r.assets[Signature], 4096)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", Signature, err)
	}
	if !Verify(u.Keys, sums, sig) {
		return nil, fmt.Errorf("release %s: %s isn't signed by the release key; not used", r.Tag, Sums)
	}
	r.Version, r.sums, err = ParseSums(sums)
	if err != nil {
		return nil, fmt.Errorf("release %s: %w", r.Tag, err)
	}
	return r, nil
}

// Verify reports whether sig (base64, or the raw 64 bytes) is a signature of message by one of keys.
func Verify(keys []ed25519.PublicKey, message, sig []byte) bool {
	raw := sig
	if len(raw) != ed25519.SignatureSize {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
		if err != nil {
			return false
		}
		raw = decoded
	}
	for _, key := range keys {
		if ed25519.Verify(key, message, raw) {
			return true
		}
	}
	return false
}

// ParseSums reads SHA256SUMS: the version from its first line and the sha256 of each file.
func ParseSums(text []byte) (string, map[string]string, error) {
	sc := bufio.NewScanner(strings.NewReader(string(text)))
	ver := ""
	sums := map[string]string{}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if v, ok := strings.CutPrefix(line, "# journal-server "); ok && ver == "" {
			ver = strings.TrimSpace(v)
			continue
		}
		hash, name, ok := strings.Cut(line, "  ")
		if !ok || len(hash) != 64 {
			return "", nil, fmt.Errorf("%s: can't read %q", Sums, line)
		}
		sums[strings.TrimPrefix(name, "*")] = strings.ToLower(hash)
	}
	if _, err := parseVersion(ver); err != nil {
		return "", nil, fmt.Errorf("%s doesn't start with \"# journal-server <version>\"", Sums)
	}
	return ver, sums, nil
}

// Newer reports whether r is newer than the running version, and whether its major version is another one (a
// change every install must follow, which isn't installed by itself).
func (u *Updater) Newer(r *Release) (newer, major bool) {
	have, err1 := parseVersion(u.Current)
	got, err2 := parseVersion(r.Version)
	if err1 != nil || err2 != nil {
		return false, false
	}
	for i := range 3 {
		if got[i] != have[i] {
			if got[i] < have[i] {
				return false, false
			}
			return true, got[0] != have[0]
		}
	}
	return false, false
}

func parseVersion(v string) ([3]int, error) {
	var n [3]int
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return n, fmt.Errorf("%q is not a version like 1.4.2", v)
	}
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return n, fmt.Errorf("%q is not a version like 1.4.2", v)
		}
		n[i] = x
	}
	return n, nil
}

// Download fetches this system's program from r into the folder of exe as <exe>.new, checks its sha256 against
// SHA256SUMS and that it runs here and says it is r's version, and returns its path.
func (u *Updater) Download(ctx context.Context, r *Release, exe string) (string, error) {
	goos, goarch := u.GOOS, u.GOARCH
	if goos == "" {
		goos, goarch = runtime.GOOS, runtime.GOARCH
	}
	name := AssetName(goos, goarch)
	url, want := r.assets[name], r.sums[name]
	if url == "" || want == "" {
		return "", fmt.Errorf("release %s has no %s", r.Version, name)
	}
	path := exe + ".new"
	if err := u.save(ctx, url, path); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if got, err := sha256Of(path); err != nil || got != want {
		os.Remove(path)
		return "", fmt.Errorf("%s: its sha256 isn't the one %s gives; not used", name, Sums)
	}
	if err := checkRuns(ctx, path, r.Version); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// checkRuns runs `<path> version` and expects it to name ver.
func checkRuns(ctx context.Context, path, ver string) error {
	if err := os.Chmod(path, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return fmt.Errorf("the new program doesn't run here: %w", err)
	}
	if !strings.HasPrefix(string(out), "journal-server "+ver) {
		return fmt.Errorf("the new program says it is %q, not %s", strings.TrimSpace(string(out)), ver)
	}
	return nil
}

// Install puts the downloaded program (from Download) in place of exe and keeps exe as <exe>.old, replacing an
// older .old. The running program keeps running from its renamed file (Windows allows renaming a running
// program, though not deleting it), so the new one runs from the next start.
func Install(exe, downloaded string) error {
	old := exe + ".old"
	if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("can't remove the previous %s: %w", filepath.Base(old), err)
	}
	if err := os.Rename(exe, old); err != nil {
		return err
	}
	if err := os.Rename(downloaded, exe); err != nil {
		if back := os.Rename(old, exe); back != nil {
			return fmt.Errorf("%v; and putting the old program back failed: %v (it is %s)", err, back, old)
		}
		return err
	}
	return nil
}

// Rollback puts <exe>.old back in place of exe, which becomes the .old, so a second rollback undoes the first.
func Rollback(exe string) error {
	old, swap := exe+".old", exe+".rollback"
	if _, err := os.Stat(old); err != nil {
		return fmt.Errorf("there is no earlier program (%s) to go back to", filepath.Base(old))
	}
	os.Remove(swap)
	if err := os.Rename(exe, swap); err != nil {
		return err
	}
	if err := os.Rename(old, exe); err != nil {
		os.Rename(swap, exe)
		return err
	}
	return os.Rename(swap, old)
}

func (u *Updater) request(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "journal-server/"+u.Current)
	req.Header.Set("Accept", "application/vnd.github+json, application/octet-stream")
	resp, err := u.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return resp, nil
}

func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	resp, err := u.request(ctx, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

func (u *Updater) save(ctx context.Context, url, path string) error {
	resp, err := u.request(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func sha256Of(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
