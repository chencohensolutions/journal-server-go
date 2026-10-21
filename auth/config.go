// Package auth holds the server's configuration and the identity of a request: the user and device
// that the client certificate named. See docs/authentication.md.
package auth

import (
	"encoding/json"
	"fmt"
	"os"
)

type UserConfig struct {
	// Also the common name (CN) of the user's client certificates: enrollment signs CN=<username>.
	Username string `json:"username"`
	// The user's journal: originals with their sidecars, stories, and the rebuildable .cache/.
	DataFolder string `json:"data_folder"`
	// Keep the previews made for this user's devices (and developed DNGs) in the data folder's .cache, so
	// asking again is instant. Off by default: devices keep the previews they asked for themselves.
	PreviewCache bool `json:"preview_cache"`
}

type AppConfig struct {
	TLSCert  string `json:"tls_cert"`
	TLSKey   string `json:"tls_key"`
	ClientCA string `json:"client_ca"`
	// Private key of ClientCA; signs the certificates issued to enrolled devices.
	ClientCAKey string `json:"client_ca_key"`
	// Address devices use to reach the server (e.g. https://192.168.50.2:56398); printed by enroll-code.
	PublicURL string `json:"public_url"`
	// Port devices connect to (default 56398); a test server beside the real one uses another.
	Port string `json:"port"`
	// Registry of enrolled devices (default devices.json).
	DevicesFile string `json:"devices_file"`
	// Loopback-only listener for the admin GraphQL API (default 127.0.0.1:8081).
	AdminAddr string `json:"admin_addr"`
	// Token for the admin API, rewritten on every server start (default admin.token).
	AdminTokenFile string `json:"admin_token_file"`
	// Path of the libvips `vips` program, used for previews; found on the PATH or in Homebrew when empty.
	Vips string `json:"vips"`
	// Path of the ffmpeg program, used for video posters (ffprobe beside it); found on the PATH or in Homebrew when empty.
	FFmpeg string `json:"ffmpeg"`
	// Most space the previews of one user may take before the least recently used are deleted (default 50).
	PreviewCacheMaxGB float64      `json:"preview_cache_max_gb"`
	Users             []UserConfig `json:"users"`
}

const (
	DefaultPort           = "56398"
	DefaultDevicesFile    = "devices.json"
	DefaultAdminAddr      = "127.0.0.1:8081"
	DefaultAdminTokenFile = "admin.token"
	DefaultPreviewCacheGB = 50
)

// LoadConfig reads config.json (or another file) and fills in the defaults.
func LoadConfig(path string) (AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return AppConfig{}, fmt.Errorf("failed to open %s: %w", path, err)
	}
	var config AppConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return AppConfig{}, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	// The username is the identity in every client certificate, so it must name one user only.
	seen := map[string]bool{}
	for _, user := range config.Users {
		if user.Username == "" {
			return AppConfig{}, fmt.Errorf("%s: a user has no username", path)
		}
		if seen[user.Username] {
			return AppConfig{}, fmt.Errorf("%s: user %q appears twice", path, user.Username)
		}
		seen[user.Username] = true
	}
	return config.WithDefaults(), nil
}

// WithDefaults fills in the optional settings.
func (c AppConfig) WithDefaults() AppConfig {
	if c.Port == "" {
		c.Port = DefaultPort
	}
	if c.DevicesFile == "" {
		c.DevicesFile = DefaultDevicesFile
	}
	if c.AdminAddr == "" {
		c.AdminAddr = DefaultAdminAddr
	}
	if c.AdminTokenFile == "" {
		c.AdminTokenFile = DefaultAdminTokenFile
	}
	if c.PreviewCacheMaxGB <= 0 {
		c.PreviewCacheMaxGB = DefaultPreviewCacheGB
	}
	return c
}

func (c AppConfig) FindUser(username string) *UserConfig {
	for i := range c.Users {
		if c.Users[i].Username == username {
			return &c.Users[i]
		}
	}
	return nil
}

// DataFolderOf is the data folder of username, for the scripts that work on one user's folder.
func (c AppConfig) DataFolderOf(username string) (string, error) {
	user := c.FindUser(username)
	if user == nil {
		return "", fmt.Errorf("user %q is not in config.json", username)
	}
	if user.DataFolder == "" {
		return "", fmt.Errorf("user %q has no data_folder in config.json", username)
	}
	return user.DataFolder, nil
}
