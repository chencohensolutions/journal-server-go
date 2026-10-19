package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/enroll"
	"github.com/journal/server-go/graph/generated"
	"github.com/journal/server-go/graph/resolver"
	"github.com/journal/server-go/media"
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

// identify attaches the client IP and, when the client presented a certificate, its user.
// Requests without a certificate pass through anonymously only when allowAnonymous is set;
// a certificate that is unknown or revoked is always rejected.
func identify(config auth.AppConfig, registry *enroll.Registry, allowAnonymous bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		ctx := auth.WithClientIP(r.Context(), ip)

		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			if !allowAnonymous {
				log.Printf("Client connection rejected: no client certificate from %s", r.RemoteAddr)
				http.Error(w, "client certificate required", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		clientCert := r.TLS.PeerCertificates[0]
		commonName := clientCert.Subject.CommonName
		currentUser := config.FindUserByCommonName(commonName)
		if currentUser == nil {
			log.Printf("Failed login: unknown client (cert CN '%s') from %s", commonName, r.RemoteAddr)
			http.Error(w, "unauthorized client certificate", http.StatusUnauthorized)
			return
		}
		if registry.IsRevoked(enroll.SerialString(clientCert.SerialNumber)) {
			log.Printf("Failed login: revoked certificate %x (user %s) from %s", clientCert.SerialNumber, currentUser.Username, r.RemoteAddr)
			http.Error(w, "client certificate revoked", http.StatusUnauthorized)
			return
		}

		if device, ok := registry.Find(enroll.SerialString(clientCert.SerialNumber)); ok {
			ctx = auth.WithDeviceName(ctx, device.Name)
		}
		log.Printf("Client connected: username=%s from %s (%s %s)", currentUser.Username, r.RemoteAddr, r.Method, r.URL.Path)
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, auth.UserContextKey, currentUser)))
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
		// Devices that are enrolling have no certificate yet. A certificate that is sent is still
		// verified against the client CA; identify and auth.RequireUserUnlessPublic decide access.
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  clientCAPool,
		MinVersion: tls.VersionTLS12,
	}, nil
}

func newGraphQLServer(enrollment *enroll.Service, library *media.Library) *handler.Server {
	srv := handler.New(generated.NewExecutableSchema(generated.Config{
		Resolvers:  &resolver.Resolver{Enrollment: enrollment, Media: library},
		Directives: generated.DirectiveRoot{Public: auth.PassThroughDirective},
	}))
	srv.AddTransport(transport.Options{})
	srv.AddTransport(transport.GET{})
	srv.AddTransport(transport.POST{})
	srv.AddTransport(transport.MultipartForm{})
	srv.SetQueryCache(lru.New[*ast.QueryDocument](1000))
	// Introspection is a root field too, so anonymous clients cannot read the schema.
	srv.Use(extension.Introspection{})
	srv.Use(extension.AutomaticPersistedQuery{Cache: lru.New[string](100)})
	srv.AroundRootFields(auth.RequireUserUnlessPublic)
	return srv
}

// GraphQL requests are small; uploads and video streams have their own handlers without these limits.
const (
	queryReadTimeout  = 30 * time.Second
	queryWriteTimeout = 2 * time.Minute
)

// withDeadlines bounds how long one request may take to arrive and to be answered.
func withDeadlines(read, write time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		rc.SetReadDeadline(time.Now().Add(read))
		rc.SetWriteDeadline(time.Now().Add(write))
		next.ServeHTTP(w, r)
	})
}

func newMux(config auth.AppConfig, enrollment *enroll.Service, library *media.Library) *http.ServeMux {
	mux := http.NewServeMux()
	playgroundHandler := playground.Handler("GraphQL playground", "/query")
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Request to / from %s: %s %s", r.RemoteAddr, r.Method, r.URL.Path)
		playgroundHandler.ServeHTTP(w, r)
	}))
	mux.Handle("/query", withDeadlines(queryReadTimeout, queryWriteTimeout,
		identify(config, enrollment.Registry, true, newGraphQLServer(enrollment, library))))

	userStore := func(r *http.Request) *media.Store {
		if user := auth.FromContext(r.Context()); user != nil {
			return library.Store(user.Username)
		}
		return nil
	}
	upload := identify(config, enrollment.Registry, false, media.UploadHandler(userStore))
	mux.Handle("PUT /upload/{sha256}", upload)
	mux.Handle("HEAD /upload/{sha256}", upload)
	mux.Handle("GET /media/{sha256}", identify(config, enrollment.Registry, false, media.OriginalHandler(userStore)))
	mux.Handle("/images/", identify(config, enrollment.Registry, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	return mux
}

// openLibrary opens the data folder of every user that has one configured.
func openLibrary(config auth.AppConfig) (*media.Library, error) {
	folders := map[string]string{}
	for _, user := range config.Users {
		if user.DataFolder != "" {
			folders[user.Username] = user.DataFolder
		} else {
			log.Printf("User %s has no data_folder in config.json; uploads are refused for them", user.Username)
		}
	}
	return media.OpenLibrary(folders, time.Now)
}

func serve(config auth.AppConfig) {
	tlsConfig, err := createTLSConfig(config)
	if err != nil {
		log.Fatalf("failed to create TLS config: %v", err)
	}

	serverCert, err := tls.LoadX509KeyPair(config.TLSCert, config.TLSKey)
	if err != nil {
		log.Fatalf("failed to load server TLS certificate: %v", err)
	}
	tlsConfig.Certificates = []tls.Certificate{serverCert}

	if config.ClientCAKey == "" {
		log.Fatalf("config.json: set client_ca_key to the private key of %s (signs enrolled devices)", config.ClientCA)
	}
	issuer, err := enroll.LoadIssuer(config.ClientCA, config.ClientCAKey, time.Now)
	if err != nil {
		log.Fatalf("failed to load client CA for enrollment: %v", err)
	}
	registry, err := enroll.OpenRegistry(config.DevicesFile)
	if err != nil {
		log.Fatalf("failed to open device registry: %v", err)
	}
	enrollment := enroll.NewService(issuer, registry, time.Now)

	library, err := openLibrary(config)
	if err != nil {
		log.Fatalf("failed to open data folders: %v", err)
	}
	go func() {
		for ; ; time.Sleep(time.Hour) {
			library.PurgeExpiredUploads()
		}
	}()

	go serveAdmin(config, enrollment)

	server := &http.Server{
		Addr:      ":" + defaultPort,
		Handler:   newMux(config, enrollment, library),
		TLSConfig: tlsConfig,
		// No overall read or write timeout: uploads and video streams take as long as they take.
		// The handlers set their own deadlines.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ln, err := net.Listen("tcp", server.Addr)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	ln = &loggingListener{Listener: ln}

	log.Printf("Listening on https://localhost:%s", defaultPort)
	log.Fatal(server.Serve(tls.NewListener(ln, tlsConfig)))
}
