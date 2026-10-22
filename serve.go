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
	"runtime/debug"
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
	"github.com/journal/server-go/search"
	"github.com/vektah/gqlparser/v2/ast"
)

// GraphQL requests are small; uploads and video streams have their own handlers without these limits.
const (
	queryReadTimeout  = 30 * time.Second
	queryWriteTimeout = 2 * time.Minute
)

// h2ReceiveWindow is how much of a request body (an upload piece) may be in flight over HTTP/2 before the server
// acknowledges it. Go's default of 1 MB caps a piece at one megabyte per round trip, about 20 MB/s on a 50 ms
// link; the most allowed is just under 4 MB.
const h2ReceiveWindow = 3 << 20

func serve(config auth.AppConfig) {
	tlsConfig, err := createTLSConfig(config)
	if err != nil {
		log.Fatalf("failed to create TLS config: %v", err)
	}
	enrollment, err := openEnrollment(config)
	if err != nil {
		log.Fatal(err)
	}
	library, err := openLibrary(config)
	if err != nil {
		log.Fatalf("failed to open data folders: %v", err)
	}
	go maintainLibrary(config, library)
	go serveAdmin(config, enrollment)
	previews := newPreviews(config)
	searcher := newSearch(config, library, previews)
	if searcher != nil {
		go searcher.Run(context.Background())
	}

	server := &http.Server{
		Addr:      ":" + config.Port,
		Handler:   newMux(config, enrollment, library, previews, searcher),
		TLSConfig: tlsConfig,
		HTTP2:     &http.HTTP2Config{MaxReceiveBufferPerStream: h2ReceiveWindow, MaxReceiveBufferPerConnection: h2ReceiveWindow},
		// No overall read or write timeout: uploads and video streams take as long as they take.
		// The handlers set their own deadlines.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	ln, err := net.Listen("tcp", server.Addr)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	log.Printf("Listening on https://0.0.0.0:%s", config.Port)
	log.Fatal(server.Serve(tls.NewListener(&loggingListener{Listener: ln}, tlsConfig)))
}

// serverVersion is the build's commit and time from the Go build info ("3f2a9c1 2026-10-23", "-dirty" with
// uncommitted changes), or "dev" when built without version control (go run from a copy, for example).
func serverVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var revision, at string
	dirty := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.time":
			at = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if revision == "" {
		return "dev"
	}
	if len(revision) > 7 {
		revision = revision[:7]
	}
	if len(at) >= 10 {
		revision += " " + at[:10]
	}
	if dirty {
		revision += " dirty"
	}
	return revision
}

// createTLSConfig loads the server certificate and trusts the client CA for client certificates.
func createTLSConfig(config auth.AppConfig) (*tls.Config, error) {
	caBytes, err := os.ReadFile(config.ClientCA)
	if err != nil {
		return nil, fmt.Errorf("failed to read client CA file: %w", err)
	}
	clientCAPool := x509.NewCertPool()
	if !clientCAPool.AppendCertsFromPEM(caBytes) {
		return nil, fmt.Errorf("failed to append client CA certificates")
	}
	serverCert, err := tls.LoadX509KeyPair(config.TLSCert, config.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("failed to load server TLS certificate: %w", err)
	}
	return tlsConfigFor(serverCert, clientCAPool), nil
}

// tlsConfigFor is the server's TLS setup, shared with the tests.
func tlsConfigFor(serverCert tls.Certificate, clientCAs *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		// Devices that are enrolling have no certificate yet. A certificate that is sent is still
		// verified against the client CA; identify and auth.RequireUserUnlessPublic decide access.
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  clientCAs,
		MinVersion: tls.VersionTLS12,
		// Offer HTTP/2: a device then keeps one connection, with one mTLS handshake (a signature in its secure
		// hardware), and fetches previews, feed pages and upload pieces side by side on it. http.Server sets h2 up
		// only when it is named here, since the listener is made from this config (Serve, not ServeTLS).
		NextProtos: []string{"h2", "http/1.1"},
	}
}

// openEnrollment loads the client CA's key (which signs enrolled devices) and the device registry.
func openEnrollment(config auth.AppConfig) (*enroll.Service, error) {
	if config.ClientCAKey == "" {
		return nil, fmt.Errorf("config.json: set client_ca_key to the private key of %s (signs enrolled devices)", config.ClientCA)
	}
	issuer, err := enroll.LoadIssuer(config.ClientCA, config.ClientCAKey, time.Now)
	if err != nil {
		return nil, fmt.Errorf("failed to load client CA for enrollment: %w", err)
	}
	registry, err := enroll.OpenRegistry(config.DevicesFile)
	if err != nil {
		return nil, fmt.Errorf("failed to open device registry: %w", err)
	}
	return enroll.NewService(issuer, registry, time.Now), nil
}

// newMux is the routing table. Every route but the playground goes through identify; only /query lets
// anonymous clients in (for the @public enrollment fields). The media routes need a certificate.
func newMux(config auth.AppConfig, enrollment *enroll.Service, library *media.Library, previews *media.Previews, searcher *search.Service) *http.ServeMux {
	registry := enrollment.Registry
	withUser := func(next http.Handler) http.Handler { return identify(config, registry, false, next) }
	userStore := func(r *http.Request) *media.Store {
		if user := auth.FromContext(r.Context()); user != nil {
			return library.Store(user.Username)
		}
		return nil
	}

	mux := http.NewServeMux()
	mux.Handle("/", playground.Handler("GraphQL playground", "/query"))
	mux.Handle("/query", withDeadlines(queryReadTimeout, queryWriteTimeout,
		gzipped(identify(config, registry, true, newGraphQLServer(enrollment, library, previews, searcher)))))
	upload := withUser(media.UploadHandler(userStore))
	if searcher != nil {
		// Search reads a finished upload right away.
		upload = withUser(then(media.UploadHandler(userStore), searcher.Wake))
	}
	mux.Handle("PUT /upload/{sha256}", upload)
	mux.Handle("HEAD /upload/{sha256}", upload)
	mux.Handle("GET /media/{sha256}", withUser(hideFromDevice(userStore, media.OriginalHandler(userStore))))
	mux.Handle("GET /media/{sha256}/preview", withUser(hideFromDevice(userStore, media.PreviewHandler(userStore, previews))))
	mux.Handle("POST /media/{sha256}/verify", withUser(media.VerifyHandler(userStore)))
	return mux
}

func newGraphQLServer(enrollment *enroll.Service, library *media.Library, previews *media.Previews, searcher *search.Service) *handler.Server {
	srv := handler.New(generated.NewExecutableSchema(generated.Config{
		Resolvers:  &resolver.Resolver{Enrollment: enrollment, Media: library, Search: searcher, Previews: previews, Version: serverVersion()},
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
