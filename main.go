package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/graph/generated"
	"github.com/journal/server-go/graph/resolver"
	"github.com/vektah/gqlparser/v2/ast"
)

const defaultPort = "8080"

type loggingListener struct {
	net.Listener
}

func (l *loggingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	log.Printf("Client attempting to connect from %s", conn.RemoteAddr())
	return conn, nil
}

func authMiddleware(config auth.AppConfig, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			log.Printf("Client connection rejected: no client certificate from %s", r.RemoteAddr)
			http.Error(w, "client certificate required", http.StatusUnauthorized)
			return
		}

		clientCert := r.TLS.PeerCertificates[0]
		commonName := clientCert.Subject.CommonName

		var currentUser *auth.UserConfig
		for _, u := range config.Users {
			if u.CertCommonName == commonName {
				currentUser = &u
				break
			}
		}

		if currentUser == nil {
			log.Printf("Failed login: unknown client (cert CN '%s') from %s", commonName, r.RemoteAddr)
			http.Error(w, "unauthorized client certificate", http.StatusUnauthorized)
			return
		}

		log.Printf("Client connected: username=%s from %s (%s %s)", currentUser.Username, r.RemoteAddr, r.Method, r.URL.Path)

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), auth.UserContextKey, currentUser)))
	})
}

func createTLSConfig(config auth.AppConfig) (*tls.Config, error) {
	caBytes, err := os.ReadFile(config.ClientCA)
	if err != nil {
		return nil, fmt.Errorf("failed to read client CA file: %w", err)
	}

	clientCAPool := x509.NewCertPool()
	if !clientCAPool.AppendCertsFromPEM(caBytes) {
		return nil, fmt.Errorf("failed to append client CA certificates")
	}

	return &tls.Config{
		ClientAuth:               tls.RequireAndVerifyClientCert,
		ClientCAs:                clientCAPool,
		MinVersion:               tls.VersionTLS12,
		PreferServerCipherSuites: true,
	}, nil
}

func main() {
	configFile, err := os.Open("config.json")
	if err != nil {
		log.Fatalf("failed to open config.json: %v", err)
	}
	defer configFile.Close()

	var config auth.AppConfig
	if err := json.NewDecoder(configFile).Decode(&config); err != nil {
		log.Fatalf("failed to parse config.json: %v", err)
	}

	tlsConfig, err := createTLSConfig(config)
	if err != nil {
		log.Fatalf("failed to create TLS config: %v", err)
	}

	serverCert, err := tls.LoadX509KeyPair(config.TLSCert, config.TLSKey)
	if err != nil {
		log.Fatalf("failed to load server TLS certificate: %v", err)
	}
	tlsConfig.Certificates = []tls.Certificate{serverCert}

	srv := handler.NewDefaultServer(generated.NewExecutableSchema(generated.Config{
		Resolvers: &resolver.Resolver{},
	}))
	srv.AddTransport(transport.Options{})
	srv.AddTransport(transport.GET{})
	srv.AddTransport(transport.POST{})
	srv.AddTransport(transport.MultipartForm{})
	srv.SetQueryCache(lru.New[*ast.QueryDocument](1000))
	srv.Use(extension.Introspection{})
	srv.Use(extension.AutomaticPersistedQuery{Cache: lru.New[string](100)})

	mux := http.NewServeMux()
	playgroundHandler := playground.Handler("GraphQL playground", "/query")
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		
		log.Printf("Request to / from %s: %s %s", r.RemoteAddr, r.Method, r.URL.Path)
		playgroundHandler.ServeHTTP(w, r)
	}))
	mux.Handle("/query", authMiddleware(config, srv))
	mux.Handle("/images/", authMiddleware(config, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := auth.FromContext(r.Context())
		if user == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		filePath := strings.TrimPrefix(r.URL.Path, "/images/")
		filePath = filepath.Clean(filePath)
		if filePath == "." || filePath == ".." || strings.Contains(filePath, "..") {
			http.Error(w, "invalid file path", http.StatusBadRequest)
			return
		}

		fullPath := filepath.Join(user.ImageFolder, filePath)
		http.ServeFile(w, r, fullPath)
	})))

	server := &http.Server{
		Addr:      ":" + defaultPort,
		Handler:   mux,
		TLSConfig: tlsConfig,
	}

	ln, err := net.Listen("tcp", server.Addr)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	ln = &loggingListener{Listener: ln}

	log.Printf("Listening on https://localhost:%s", defaultPort)
	log.Fatal(server.Serve(tls.NewListener(ln, tlsConfig)))
}
