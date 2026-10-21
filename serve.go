package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
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

const defaultPort = "56398"

// GraphQL requests are small; uploads and video streams have their own handlers without these limits.
const (
	queryReadTimeout  = 30 * time.Second
	queryWriteTimeout = 2 * time.Minute
)

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

	server := &http.Server{
		Addr:      ":" + defaultPort,
		Handler:   newMux(config, enrollment, library, newPreviews(config)),
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
	log.Printf("Listening on https://0.0.0.0:%s", defaultPort)
	log.Fatal(server.Serve(tls.NewListener(&loggingListener{Listener: ln}, tlsConfig)))
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
	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		// Devices that are enrolling have no certificate yet. A certificate that is sent is still
		// verified against the client CA; identify and auth.RequireUserUnlessPublic decide access.
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  clientCAPool,
		MinVersion: tls.VersionTLS12,
	}, nil
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
func newMux(config auth.AppConfig, enrollment *enroll.Service, library *media.Library, previews *media.Previews) *http.ServeMux {
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
		gzipped(identify(config, registry, true, newGraphQLServer(enrollment, library)))))
	upload := withUser(media.UploadHandler(userStore))
	mux.Handle("PUT /upload/{sha256}", upload)
	mux.Handle("HEAD /upload/{sha256}", upload)
	mux.Handle("GET /media/{sha256}", withUser(media.OriginalHandler(userStore)))
	mux.Handle("GET /media/{sha256}/preview", withUser(media.PreviewHandler(userStore, previews)))
	mux.Handle("POST /media/{sha256}/verify", withUser(media.VerifyHandler(userStore)))
	return mux
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
