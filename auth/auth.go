package auth

import "context"

type ContextKey string

const UserContextKey ContextKey = "user"

type UserConfig struct {
	Username       string `json:"username"`
	CertCommonName string `json:"cert_common_name"`
	ImageFolder    string `json:"image_folder"`
}

type AppConfig struct {
	TLSCert  string       `json:"tls_cert"`
	TLSKey   string       `json:"tls_key"`
	ClientCA string       `json:"client_ca"`
	Users    []UserConfig `json:"users"`
}

func FromContext(ctx context.Context) *UserConfig {
	user, _ := ctx.Value(UserContextKey).(*UserConfig)
	return user
}
