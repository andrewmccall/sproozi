// Package proxytransport terminates inspected HTTPS sessions at a trusted gateway.
package proxytransport

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/andrewmccall/sproozi/internal/gateway"
)

const (
	proxyAuthorizationHeader = "Proxy-Authorization"
)

// Identity returns the authenticated, immutable session identity. Semantic handlers
// must authorize their precise operation against live policy using this identity.
func Identity(ctx context.Context) *gateway.ExecutionGrant {
	identity, _ := gateway.IdentityFromContext(ctx)
	return identity
}

type Config struct {
	// Certificates maps exact DNS names (without port) to inspection certificates.
	Certificates     map[string]tls.Certificate
	Authenticate     func(context.Context, string) (*gateway.ExecutionGrant, error)
	Revalidate       func(context.Context, *gateway.ExecutionGrant) error
	Handler          http.Handler
	AllowedAuthority func(string) bool
	// RevocationInterval bounds idle/in-flight sessions after revocation.
	RevocationInterval time.Duration
	SessionLifetime    time.Duration
	// AuthorityTimeout bounds authentication and each live authority lookup.
	AuthorityTimeout        time.Duration
	MaxConnections          int
	MaxSessionsPerExecution int
}

type Gateway struct {
	config         Config
	connections    chan struct{}
	requests       chan struct{}
	authorityCalls chan struct{}
	sessionsMu     sync.Mutex
	sessions       map[gateway.Reference]int
}

func New(c Config) (*Gateway, error) {
	if len(c.Certificates) == 0 || len(c.Certificates) > 64 || c.Authenticate == nil || c.Revalidate == nil || c.Handler == nil {
		return nil, fmt.Errorf("inspection requires bounded hosts, identity, revalidation and semantic handler")
	}
	certificates := make(map[string]tls.Certificate, len(c.Certificates))
	for host, cert := range c.Certificates {
		if host == "" || strings.ToLower(host) != host || strings.ContainsAny(host, ":/*@%") || net.ParseIP(host) != nil {
			return nil, fmt.Errorf("inspection host must be an exact DNS name")
		}
		certificates[host] = cert
	}
	c.Certificates = certificates
	if c.RevocationInterval == 0 {
		c.RevocationInterval = time.Second
	}
	if c.SessionLifetime == 0 {
		c.SessionLifetime = 5 * time.Minute
	}
	if c.RevocationInterval < 10*time.Millisecond || c.RevocationInterval > 5*time.Second || c.SessionLifetime < time.Second || c.SessionLifetime > 5*time.Minute {
		return nil, fmt.Errorf("invalid session bounds")
	}
	if c.AuthorityTimeout == 0 {
		c.AuthorityTimeout = 5 * time.Second
	}
	if c.MaxConnections == 0 {
		c.MaxConnections = 128
	}
	if c.MaxSessionsPerExecution == 0 {
		c.MaxSessionsPerExecution = min(8, c.MaxConnections)
	}
	if c.AuthorityTimeout < 10*time.Millisecond || c.AuthorityTimeout > 10*time.Second ||
		c.MaxConnections < 1 || c.MaxConnections > 4096 || c.MaxSessionsPerExecution < 1 ||
		c.MaxSessionsPerExecution > 128 || c.MaxSessionsPerExecution > c.MaxConnections {
		return nil, fmt.Errorf("invalid authority or admission bounds")
	}
	return &Gateway{config: c, connections: make(chan struct{}, c.MaxConnections),
		requests: make(chan struct{}, c.MaxConnections), authorityCalls: make(chan struct{}, c.MaxConnections),
		sessions: make(map[gateway.Reference]int)}, nil
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !acquire(g.requests) {
		http.Error(w, "gateway capacity unavailable", http.StatusServiceUnavailable)
		return
	}
	defer func() { <-g.requests }()
	if r.Method != http.MethodConnect {
		http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
		return
	}
	host, port, err := net.SplitHostPort(r.Host)
	certificate, allowed := g.config.Certificates[host]
	if err != nil || !allowed || (g.config.AllowedAuthority == nil && port != "443") || (g.config.AllowedAuthority != nil && !g.config.AllowedAuthority(r.Host)) || (r.URL.Host != "" && r.URL.Host != r.Host) || (r.URL.Opaque != "" && r.URL.Opaque != r.Host) {
		http.Error(w, "unsupported destination", http.StatusForbidden)
		return
	}
	auth := r.Header.Values(proxyAuthorizationHeader)
	token, valid := proxyToken(auth)
	if !valid {
		http.Error(w, "gateway identity required", http.StatusUnauthorized)
		return
	}
	identity, err := authorityCall(r.Context(), g, func(ctx context.Context) (*gateway.ExecutionGrant, error) {
		return g.config.Authenticate(ctx, token)
	})
	if err != nil || identity.Validate() != nil {
		http.Error(w, "invalid gateway identity", http.StatusUnauthorized)
		return
	}
	identity = identity.Clone()
	if g.revalidate(r.Context(), identity) != nil {
		http.Error(w, "revoked gateway identity", http.StatusForbidden)
		return
	}
	if !g.acquireSession(identity.Execution) {
		http.Error(w, "execution session capacity unavailable", http.StatusTooManyRequests)
		return
	}
	defer g.releaseSession(identity.Execution)
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "unsupported transport", http.StatusInternalServerError)
		return
	}
	connection, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = connection.Close() }()
	ctx, cancel := context.WithTimeout(gateway.WithIdentity(r.Context(), identity), g.config.SessionLifetime)
	defer cancel()
	deadline, _ := ctx.Deadline()
	_ = connection.SetDeadline(minDeadline(deadline, time.Now().Add(10*time.Second)))
	// No TLS bytes should precede CONNECT acknowledgement. Reject instead of losing
	// buffered bytes or accidentally treating them as an uninspected protocol.
	if buffered.Reader.Buffered() != 0 {
		return
	}
	_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
	if buffered.Flush() != nil {
		return
	}
	tlsConnection := tls.Server(connection, &tls.Config{
		MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if hello.ServerName != host {
				return nil, fmt.Errorf("SNI does not match CONNECT target")
			}
			return &certificate, nil
		},
	})
	if tlsConnection.HandshakeContext(ctx) != nil {
		return
	}
	_ = connection.SetDeadline(deadline)
	listener := newSessionListener(tlsConnection, connection)
	defer func() { _ = listener.Close() }()
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(g.config.RevocationInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = listener.Close()
				return
			case <-ticker.C:
				if g.revalidate(ctx, identity) != nil {
					cancel()
					_ = listener.Close()
					return
				}
			}
		}
	}()
	defer func() { cancel(); _ = listener.Close(); <-monitorDone }()
	server := &http.Server{
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			g.serveInspected(w, request, identity, host, port)
		}),
	}
	_ = server.Serve(listener)
}

func (g *Gateway) serveInspected(
	w http.ResponseWriter, request *http.Request, identity *gateway.ExecutionGrant, host, port string,
) {
	if g.revalidate(request.Context(), identity) != nil {
		request.Close = true
		http.Error(w, "revoked gateway identity", http.StatusForbidden)
		return
	}
	if (request.Host != net.JoinHostPort(host, port) && (port != "443" || request.Host != host)) || request.URL.IsAbs() || request.Method == http.MethodConnect || request.Header.Get("Upgrade") != "" || request.Header.Get("Transfer-Encoding") != "" || len(request.TransferEncoding) != 0 {
		request.Close = true
		http.Error(w, "unsupported inspected request", http.StatusForbidden)
		return
	}
	// Client authentication is never provider authority, even when the local
	// adapter has been replaced by attacker-controlled code.
	request.Header.Del("Authorization")
	request.Header.Del(proxyAuthorizationHeader)
	request.Header.Del("Cookie")
	request.URL.Scheme, request.URL.Host = "https", net.JoinHostPort(host, port)
	g.config.Handler.ServeHTTP(w, request)
}

// proxyToken accepts Bearer for explicit fixtures and Basic user-info emitted
// by ordinary HTTP clients. Only the fixed username is accepted; credentials
// are converted to the internal token and never forwarded upstream.
func proxyToken(values []string) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	value := values[0]
	if after, ok := strings.CutPrefix(value, "Bearer "); ok {
		token := after
		return token, token != ""
	}
	if !strings.HasPrefix(value, "Basic ") {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, "Basic "))
	if err != nil {
		return "", false
	}
	user, token, ok := strings.Cut(string(raw), ":")
	return token, ok && user == "sproozi" && token != "" && !strings.ContainsAny(token, "\r\n")
}

type sessionListener struct {
	connection net.Conn
	transport  net.Conn
	mu         sync.Mutex
	accepted   bool
	done       chan struct{}
	once       sync.Once
}

func newSessionListener(connection, transport net.Conn) *sessionListener {
	return &sessionListener{connection: connection, transport: transport, done: make(chan struct{})}
}
func (l *sessionListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.accepted {
		l.accepted = true
		l.mu.Unlock()
		return &sessionConnection{Conn: l.connection, close: l.Close}, nil
	}
	l.mu.Unlock()
	<-l.done
	return nil, net.ErrClosed
}
func (l *sessionListener) Addr() net.Addr { return l.connection.LocalAddr() }
func (l *sessionListener) Close() error {
	// Revocation closes the underlying carrier directly: TLS Close may wait
	// for a close-notify writer, which is not part of the authority deadline.
	l.once.Do(func() { _ = l.transport.Close(); close(l.done) })
	return nil
}

type sessionConnection struct {
	net.Conn
	close func() error
}

func (c *sessionConnection) Close() error { return c.close() }
