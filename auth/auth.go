package auth

import "context"

type ContextKey string

const (
	UserContextKey     ContextKey = "user"
	ClientIPContextKey ContextKey = "client_ip"
)

type UserConfig struct {
	Username       string `json:"username"`
	CertCommonName string `json:"cert_common_name"`
	ImageFolder    string `json:"image_folder"`
}

type AppConfig struct {
	TLSCert  string `json:"tls_cert"`
	TLSKey   string `json:"tls_key"`
	ClientCA string `json:"client_ca"`
	// Private key of ClientCA; signs the certificates issued to enrolled devices.
	ClientCAKey string `json:"client_ca_key"`
	// Address devices use to reach the server (e.g. https://192.168.50.2:8080); printed by enroll-code.
	PublicURL string `json:"public_url"`
	// Registry of enrolled devices (default devices.json).
	DevicesFile string `json:"devices_file"`
	// Loopback-only listener for the admin GraphQL API (default 127.0.0.1:8081).
	AdminAddr string `json:"admin_addr"`
	// Token for the admin API, rewritten on every server start (default admin.token).
	AdminTokenFile string       `json:"admin_token_file"`
	Users          []UserConfig `json:"users"`
}

const (
	DefaultDevicesFile    = "devices.json"
	DefaultAdminAddr      = "127.0.0.1:8081"
	DefaultAdminTokenFile = "admin.token"
)

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

// FromContext returns the user authenticated by client certificate, or nil for an anonymous request.
func FromContext(ctx context.Context) *UserConfig {
	user, _ := ctx.Value(UserContextKey).(*UserConfig)
	return user
}

func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, ClientIPContextKey, ip)
}

func ClientIPFromContext(ctx context.Context) string {
	ip, _ := ctx.Value(ClientIPContextKey).(string)
	return ip
}
