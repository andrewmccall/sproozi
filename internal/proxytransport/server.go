package proxytransport

import (
	"crypto/tls"
	"net/http"
	"time"
)

// NewServer applies the common outer TLS, HTTP/1 and parsing limits. The caller
// must wrap its raw listener with Gateway.LimitListener before serving TLS.
// Certificate replacements apply to new connections.
func NewServer(address string, certificate *Certificate, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    32 << 10,
		// CONNECT inspection takes ownership of the HTTP/1.1 connection after
		// authentication. Disable HTTP/2 so every accepted proxy request has the
		// same hijackable transport semantics across clients.
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
		TLSConfig: &tls.Config{
			MinVersion:     tls.VersionTLS13,
			GetCertificate: certificate.get,
		},
	}
}
