package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/enroll"
	"github.com/journal/server-go/graph/admin/generated"
	"github.com/journal/server-go/graph/admin/resolver"
)

// serveAdmin runs the admin GraphQL API on a loopback-only listener. Callers prove they can read
// the token file, which is rewritten (owner-only) on every start.
func serveAdmin(config auth.AppConfig, enrollment *enroll.Service, refreshIndexes func()) {
	if err := requireLoopback(config.AdminAddr); err != nil {
		log.Fatalf("admin_addr: %v", err)
	}
	token, err := writeAdminToken(config.AdminTokenFile)
	if err != nil {
		log.Fatalf("failed to write admin token: %v", err)
	}

	log.Printf("Admin API listening on http://%s/admin/query", config.AdminAddr)
	server := &http.Server{
		Addr:              config.AdminAddr,
		Handler:           newAdminMux(config, enrollment, token, refreshIndexes),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(server.ListenAndServe())
}

func newAdminMux(config auth.AppConfig, enrollment *enroll.Service, token string, refreshIndexes func()) *http.ServeMux {
	srv := handler.New(generated.NewExecutableSchema(generated.Config{
		Resolvers: &resolver.Resolver{Config: config, Enrollment: enrollment, Now: time.Now, RefreshIndexes: refreshIndexes},
	}))
	srv.AddTransport(transport.POST{})

	mux := http.NewServeMux()
	mux.Handle("/admin/query", requireAdminToken(token, srv))
	return mux
}

func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("%s is not a loopback address", addr)
	}
	return nil
}

func writeAdminToken(path string) (string, error) {
	raw := make([]byte, 32)
	rand.Read(raw)
	token := hex.EncodeToString(raw)
	// Remove first so an existing file with looser permissions is not reused.
	os.Remove(path)
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

func readAdminToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s (is the server running?): %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

func requireAdminToken(token string, next http.Handler) http.Handler {
	expected := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), expected) != 1 {
			http.Error(w, "invalid admin token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
