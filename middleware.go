package main

import (
	"compress/gzip"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/enroll"
	"github.com/journal/server-go/media"
)

// identify attaches the client IP and, when the client presented a certificate, its user and device.
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
		currentUser := config.FindUser(commonName)
		if currentUser == nil {
			log.Printf("Failed login: unknown client (cert CN '%s') from %s", commonName, r.RemoteAddr)
			http.Error(w, "unauthorized client certificate", http.StatusUnauthorized)
			return
		}
		serial := enroll.SerialString(clientCert.SerialNumber)
		if registry.IsRevoked(serial) {
			log.Printf("Failed login: revoked certificate %s (user %s) from %s", serial, currentUser.Username, r.RemoteAddr)
			http.Error(w, "client certificate revoked", http.StatusUnauthorized)
			return
		}
		if device, ok := registry.Find(serial); ok {
			ctx = auth.WithDeviceName(ctx, device.Name)
			if device.WithholdPrivate {
				ctx = auth.WithWithholdPrivate(ctx)
			}
		}
		log.Printf("Client connected: username=%s from %s (%s %s)", currentUser.Username, r.RemoteAddr, r.Method, r.URL.Path)
		next.ServeHTTP(w, r.WithContext(auth.WithUser(ctx, currentUser)))
	})
}

// hideFromDevice answers 404 for a file the device may not see (media.View: a hidden collection's, or a private
// file to a device kept from them), as if the server didn't have it.
func hideFromDevice(lookup media.StoreLookup, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if store := lookup(r); store != nil {
			// Find: a JPEG kept as JPEG XL, asked for by the JPEG's sha256, is hidden as its JPEG XL is.
			if item, ok := store.Find(r.PathValue("sha256")); ok && store.Hides(store.ViewFor(auth.WithholdPrivateFromContext(r.Context())), item) {
				http.NotFound(w, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// gzipped compresses responses for clients that accept it: the change feed of a large library is mostly
// repeated field names, and shrinks about fivefold.
func gzipped(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		defer zw.Close()
		next.ServeHTTP(gzipWriter{ResponseWriter: w, writer: zw}, r)
	})
}

type gzipWriter struct {
	http.ResponseWriter
	writer *gzip.Writer
}

func (g gzipWriter) Write(b []byte) (int, error) { return g.writer.Write(b) }

func (g gzipWriter) WriteHeader(status int) {
	g.ResponseWriter.Header().Del("Content-Length")
	g.ResponseWriter.WriteHeader(status)
}

func (g gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

// then runs each of after once next has answered.
func then(next http.Handler, after ...func()) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		for _, fn := range after {
			fn()
		}
	})
}

// withDeadlines bounds how long one request may take to arrive and to be answered.
func withDeadlines(read, write time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		rc.SetReadDeadline(time.Now().Add(read))
		rc.SetWriteDeadline(time.Now().Add(write))
		next.ServeHTTP(w, r)
	})
}

// loggingListener logs every connection as it is accepted, before TLS: a client whose certificate the
// handshake rejects still shows up here.
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
