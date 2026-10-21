// Package auth holds the server's configuration and the identity of a request: the user and device
// that the client certificate named. See docs/authentication.md.
package auth

import (
	"encoding/json"
	"fmt"
	"os"
)

type UserConfig struct {
	Username       string `json:"username"`
	CertCommonName string `json:"cert_common_name"`
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
	return config.WithDefaults(), nil
}

// WithDefaults fills in the optional settings.
func (c AppConfig) WithDefaults() AppConfig {
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

func (c AppConfig) FindUserByCommonName(commonName string) *UserConfig {
	for i := range c.Users {
		if c.Users[i].CertCommonName == commonName {
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
