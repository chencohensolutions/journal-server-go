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
	"os/signal"
	"syscall"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/copies"
	"github.com/journal/server-go/enroll"
	"github.com/journal/server-go/graph/generated"
	"github.com/journal/server-go/graph/resolver"
	"github.com/journal/server-go/localindex"
	"github.com/journal/server-go/media"
	"github.com/journal/server-go/peers"
	"github.com/journal/server-go/version"
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

// serve runs the server until ctx ends, Ctrl+C or a stop from the system, then calls stopped (exitStopped by
// default) once the reader has finished the file in hand and the index caches are saved.
func serve(ctx context.Context, config auth.AppConfig, stopped func()) {
	log.Printf("journal-server %s", version.String())
	ctx, stopForUpdate := context.WithCancel(ctx)
	defer stopForUpdate()
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
	previews := newPreviews(config)
	readCamerasWith(library, previews)
	loadJXL(config, library)
	ix := openIndexes(config, library, previews)
	var refreshIndexes func()
	if ix.daily {
		refreshIndexes = ix.refresh
	}
	readerDone := make(chan struct{})
	running, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	var daily *dailyRuns
	if ix.daily {
		daily = newDailyRuns(running, config, ix.refresh)
		ix.runs = daily
	}
	copiers, err := newCopiers(config, library)
	if err != nil {
		log.Fatalf("config.json copies: %v", err)
	}
	copySet := startCopies(running, library, copiers)
	go renewPeersDaily(running, config, copySet.all)
	host, err := openPeerHost(config, enrollment.Issuer)
	if err != nil {
		log.Fatal(err)
	}
	unmapped := make(chan struct{}) // the router's port mapping is removed on stop, before the process ends
	if d, err := openDiscovery(config); err != nil {
		log.Printf("Discovery is off: %v", err)
		close(unmapped)
	} else {
		thisServer.Store(d)
		if host != nil {
			host.Discovery = d.forPeers
		}
		mapPort(running, config, d, unmapped)
		go d.publisher.Run(running)
		go askPeerAddressesEvery(running, config, d)
	}
	swaps := newSwapService(config, library, host, copySet, selfFromDiscovery(config))
	go serveAdmin(config, enrollment, refreshIndexes, copySet.all, func(c *copies.Copier) { runCopy(running, c) }, host)
	switch {
	case ix.daily:
		go func() { indexDaily(running, daily, library.Users()); close(readerDone) }()
	case ix.reader != nil:
		// The server's own reader writes the indexes' caches: journal-server index can't run beside it.
		if err := lockIndexes(library); err != nil {
			log.Printf("Reading new files is off until a restart: %v", err)
			close(readerDone)
			break
		}
		ix.runs = readerRuns{ix.reader}
		go func() { ix.reader.Run(running); close(readerDone) }()
	default:
		close(readerDone)
	}
	go stopOnSignal(running, stop, bothDone(readerDone, unmapped), library, stopped)
	go updateDaily(running, config, func() { restarting.Store(true); stopForUpdate() })

	server := &http.Server{
		Addr:      ":" + config.Port,
		Handler:   newMux(config, enrollment, library, previews, ix, copyRunner{running, copySet}, host, swaps),
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

// stopOnSignal waits for Ctrl+C (or a stop from the system), lets the reader (or a daily run of journal-server
// index) finish the file in hand, saves the
// index caches the server's writes made stale, so the next start doesn't read the library's sidecars again, and
// calls stopped. A second Ctrl+C exits at once.
func stopOnSignal(running context.Context, stop func(), readerDone <-chan struct{}, library *media.Library, stopped func()) {
	<-running.Done()
	stop() // a second Ctrl+C now ends the process the usual way
	log.Printf("Stopping: finishing the file being read, then saving the index caches (Ctrl+C again to stop now)")
	select {
	case <-readerDone:
	case <-time.After(30 * time.Second):
	}
	library.RefreshIndexCachesIfChanged()
	log.Printf("Stopped")
	stopped()
}

// bothDone is closed once a and b are.
func bothDone(a, b <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	go func() { <-a; <-b; close(done) }()
	return done
}

// exitStopped ends the process once the server has stopped, when it runs from a console, or starts the newer
// program an update installed.
func exitStopped() {
	if restarting.Load() {
		startAgain()
	}
	os.Exit(0)
}

// serverVersion is serverInfo.version: version/VERSION and the build's commit ("1.4.2 (3f2a9c1 2026-10-23)").
func serverVersion() string { return version.String() }

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
func newMux(config auth.AppConfig, enrollment *enroll.Service, library *media.Library, previews *media.Previews, ix indexes,
	copyRuns resolver.CopyRunner, host *peers.Host, swaps resolver.SwapService) *http.ServeMux {
	registry := enrollment.Registry
	withUser := func(next http.Handler) http.Handler { return identify(config, registry, false, next) }
	userStore := func(r *http.Request) *media.Store {
		if user := auth.FromContext(r.Context()); user != nil {
			return library.Store(user.Username)
		}
		return nil
	}

	local := newLocal(config, library, ix)
	mux := http.NewServeMux()
	mux.Handle("/", playground.Handler("GraphQL playground", "/query"))
	mux.Handle("/query", withDeadlines(queryReadTimeout, queryWriteTimeout,
		gzipped(identify(config, registry, true, newGraphQLServer(enrollment, library, previews, ix, local, copyRuns, swaps)))))
	// The nudity scan, faces and search read a finished upload right away.
	var wake []func()
	if ix.reader != nil {
		wake = append(wake, ix.reader.Wake)
	}
	upload := withUser(then(media.UploadHandler(userStore), wake...))
	mux.Handle("PUT /upload/{sha256}", upload)
	mux.Handle("HEAD /upload/{sha256}", upload)
	mux.Handle("GET /media/{sha256}", withUser(hideFromDevice(userStore, media.OriginalHandler(userStore))))
	mux.Handle("GET /media/{sha256}/preview", withUser(hideFromDevice(userStore, media.PreviewHandler(userStore, previews))))
	mux.Handle("POST /media/{sha256}/verify", withUser(media.VerifyHandler(userStore)))
	// Indexes kept on phones (docs/search.md "On the phone"): /index/people is a document, gzipped like the feed; the
	// streams gzip the text in photos themselves, and the model files go as they are, with Range.
	mux.Handle("GET /index/people", withUser(gzipped(http.HandlerFunc(local.ServePeople))))
	mux.Handle("GET /index/{name}", withUser(http.HandlerFunc(local.ServeIndex)))
	mux.Handle("GET /models/{index}/{name}", withUser(http.HandlerFunc(local.ServeModel)))
	// The peer API (docs/peers.md): other servers keeping copies here, each with a peer certificate; never identify,
	// which refuses those. Only with peers_folder.
	if host != nil {
		host.Mount(mux)
	}
	return mux
}

func newGraphQLServer(enrollment *enroll.Service, library *media.Library, previews *media.Previews, ix indexes, local *localindex.Server,
	copyRuns resolver.CopyRunner, swaps resolver.SwapService) *handler.Server {
	srv := handler.New(generated.NewExecutableSchema(generated.Config{
		Resolvers: &resolver.Resolver{Enrollment: enrollment, Library: library, Search: ix.search, Faces: ix.faces,
			TextInPhotos: ix.ocr, SimilarPhotos: ix.similar, AestheticScores: ix.aesthetic, NudityScan: ix.nudity != nil,
			Previews: previews, Version: serverVersion(), Local: local, IndexRuns: ix.runs, Copies: copyRuns,
			Discovery: forPhones, SwapSetup: swaps},
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
