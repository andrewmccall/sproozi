// Package proxytransport terminates inspected HTTPS sessions at a trusted gateway.
package proxytransport

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

const (
	proxyAuthorizationHeader = "Proxy-Authorization"
)

// Inspected-connection lifetime. One CONNECT tunnel carries a harness's
// requests for at most MaxSessionLifetime; the harness then opens a new one.
// Each request on it must end ResponseMargin before the tunnel does, so its
// handler can still reply instead of the tunnel closing mid-request.
// MaxRequestDuration is therefore the longest any single inspected request,
// such as a Kubernetes watch, can take. A model call alone may extend its
// deadline with ExtendModelCall; the tunnel then stays open for that call.
const (
	MaxSessionLifetime = 5 * time.Minute
	ResponseMargin     = 10 * time.Second
	MaxRequestDuration = MaxSessionLifetime - ResponseMargin
)

// Admission defaults and bounds. Every admitted connection is an inspected TLS
// session, so the connection limit is sized against gateway memory.
// TestInspectedSessionMemory measures one streaming session (outer ServeTLS
// connection, inspected tls.Server, upstream TLS client, 32 KiB copy buffer and
// six goroutines) at about 150 to 160 KiB of in-use heap and stack.
// InspectedSessionBytes budgets 256 KiB, at least 1.6 times that. 2048 sessions
// then hold about 512 MiB; twice that for GC headroom still leaves room for the
// watch cache and the rest of the process under the 2 GiB limit in
// config/manager/gateway.yaml, where GOMEMLIMIT is 90% of the limit. Request
// and response bodies that a handler buffers are extra, per request in flight.
// Lower both together.
const (
	// InspectedSessionBytes is the per-session memory budget; the measured
	// figure must stay below it.
	InspectedSessionBytes = 256 << 10
	DefaultMaxConnections = 2048
	MaxConnectionsLimit   = 4096
	// DefaultMaxSessionsPerExecution is the fairness guard: one execution can
	// hold at most this many inspected sessions of the shared pool.
	DefaultMaxSessionsPerExecution = 16
	MaxSessionsPerExecutionLimit   = 128
)

type Config struct {
	// Certificates maps exact DNS names (without port) to inspection certificates.
	Certificates map[string]tls.Certificate
	// Serving, when set, replaces every inspected host's certificate with its
	// current value. Certificates then lists only the inspected hosts.
	Serving          *Certificate
	Authenticate     func(context.Context, string) (*gateway.ExecutionGrant, error)
	Revalidate       func(context.Context, *gateway.ExecutionGrant) error
	Handler          http.Handler
	AllowedAuthority func(string) bool
	// AuditLogger, when set, records authenticated destination denials.
	AuditLogger audit.Writer
	// RevocationInterval bounds idle/in-flight sessions after revocation.
	RevocationInterval time.Duration
	// SessionLifetime bounds one inspected connection. Zero selects
	// MaxSessionLifetime, which is also its upper bound.
	SessionLifetime time.Duration
	// AuthorityTimeout bounds authentication and each live authority lookup.
	AuthorityTimeout time.Duration
	// MaxConnections bounds admitted TCP connections, CONNECT requests in
	// progress and pending authority lookups. Zero selects
	// DefaultMaxConnections.
	MaxConnections int
	// MaxModelCallDuration bounds one model call that its handler extends
	// with ExtendModelCall past the connection's request deadline. Zero
	// selects DefaultMaxModelCallDuration; MaxModelCallDurationLimit is its
	// upper bound.
	MaxModelCallDuration time.Duration
	// MaxSessionsPerExecution bounds inspected sessions held by one execution.
	// Zero selects DefaultMaxSessionsPerExecution, capped at MaxConnections.
	MaxSessionsPerExecution int
	// Logger receives a line when an open inspected connection is revoked,
	// a connection is refused at the per-execution limit or a request is cut
	// short by its connection's lifetime.
	// Nil selects slog.Default().
	Logger *slog.Logger
}

type Gateway struct {
	config         Config
	connections    chan struct{}
	requests       chan struct{}
	authorityCalls chan struct{}
	sessionsMu     sync.Mutex
	sessions       map[gateway.Reference]int
	revocations    *executionLog
	limitRejected  *executionLog
	authRejected   *authFailureLog
	logger         *slog.Logger
}

func New(c Config) (*Gateway, error) {
	if len(c.Certificates) == 0 || len(c.Certificates) > 64 || c.Authenticate == nil || c.Revalidate == nil || c.Handler == nil || c.AllowedAuthority == nil {
		return nil, fmt.Errorf("inspection requires bounded hosts, identity, revalidation, semantic handler and authority routing")
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
		c.SessionLifetime = MaxSessionLifetime
	}
	if c.RevocationInterval < 10*time.Millisecond || c.RevocationInterval > 5*time.Second || c.SessionLifetime < time.Second || c.SessionLifetime > MaxSessionLifetime {
		return nil, fmt.Errorf("invalid session bounds")
	}
	if c.AuthorityTimeout == 0 {
		c.AuthorityTimeout = 5 * time.Second
	}
	if c.MaxConnections == 0 {
		c.MaxConnections = DefaultMaxConnections
	}
	if c.MaxSessionsPerExecution == 0 {
		c.MaxSessionsPerExecution = min(DefaultMaxSessionsPerExecution, c.MaxConnections)
	}
	if err := c.defaultModelCall(); err != nil {
		return nil, err
	}
	if c.AuthorityTimeout < 10*time.Millisecond || c.AuthorityTimeout > 10*time.Second ||
		c.MaxConnections < 1 || c.MaxConnections > MaxConnectionsLimit || c.MaxSessionsPerExecution < 1 ||
		c.MaxSessionsPerExecution > MaxSessionsPerExecutionLimit || c.MaxSessionsPerExecution > c.MaxConnections {
		return nil, fmt.Errorf("invalid authority or admission bounds")
	}
	logger := c.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// The request and authority-lookup pools take the connection limit too:
	// each admitted CONNECT holds one request slot for its whole session.
	return &Gateway{config: c, connections: make(chan struct{}, c.MaxConnections),
		requests: make(chan struct{}, c.MaxConnections), authorityCalls: make(chan struct{}, c.MaxConnections),
		sessions: make(map[gateway.Reference]int), revocations: newRevocationLog(c.Logger),
		limitRejected: newLimitLog(c.Logger), authRejected: newAuthFailureLog(logger), logger: logger}, nil
}

// authenticate returns the CONNECT's current execution identity, or writes
// the refusal and reports false.
func (g *Gateway) authenticate(w http.ResponseWriter, r *http.Request) (*gateway.ExecutionGrant, bool) {
	token, valid := proxyToken(r.Header.Values(proxyAuthorizationHeader))
	if !valid {
		http.Error(w, "gateway identity required", http.StatusUnauthorized)
		return nil, false
	}
	identity, err := authorityCall(r.Context(), g, func(ctx context.Context) (*gateway.ExecutionGrant, error) {
		return g.config.Authenticate(ctx, token)
	})
	if err == nil {
		err = identity.Validate()
	}
	if err != nil {
		// The client learns only that the identity was refused; the cause
		// (TokenReview failure, unbound token, Pod mismatch, run not running,
		// a Forbidden read) stays in the gateway log.
		g.authRejected.record(err, r.RemoteAddr)
		http.Error(w, "invalid gateway identity", http.StatusUnauthorized)
		return nil, false
	}
	identity = identity.Clone()
	if err := g.revalidate(r.Context(), identity); err != nil {
		g.revocations.revoked(identity.Execution, err)
		http.Error(w, "revoked gateway identity", http.StatusForbidden)
		return nil, false
	}
	return identity, true
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
	if err != nil || (r.URL.Host != "" && r.URL.Host != r.Host) || (r.URL.Opaque != "" && r.URL.Opaque != r.Host) {
		http.Error(w, "unsupported destination", http.StatusForbidden)
		return
	}
	identity, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	// Authenticate before routing so a destination denial is attributed to
	// its execution and can be diagnosed by that execution alone.
	certificate, allowed := g.config.Certificates[host]
	if !allowed || !g.config.AllowedAuthority(r.Host) {
		if g.config.AuditLogger != nil {
			event := gateway.AuditEvent(gateway.WithIdentity(r.Context(), identity), http.MethodConnect+" "+r.Host)
			event.Capability, event.Service = string(api.CapabilityNetworkEgress), "network"
			event.DenyReason = "destination " + r.Host + " is not enabled at this gateway"
			g.config.AuditLogger.Log(event)
		}
		http.Error(w, "unsupported destination", http.StatusForbidden)
		return
	}
	if !g.acquireSession(identity.Execution) {
		g.limitRejected.record(identity.Execution, g.config.MaxSessionsPerExecution)
		http.Error(w, "execution inspected-connection limit reached", http.StatusTooManyRequests)
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
	// The session ends at its lifetime, or later when a model call extended
	// with ExtendModelCall still holds it; see sessionLife.
	ctx, cancel := context.WithCancel(gateway.WithIdentity(r.Context(), identity))
	defer cancel()
	deadline := time.Now().Add(g.config.SessionLifetime)
	margin := min(ResponseMargin, g.config.SessionLifetime/4)
	life := &sessionLife{connection: connection, end: cancel, grace: margin}
	lifetime := time.AfterFunc(g.config.SessionLifetime, life.expire)
	defer lifetime.Stop()
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
			if g.config.Serving != nil {
				return g.config.Serving.get(hello)
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
	// Both the monitor and a request can see the revocation; log it once.
	var revokedOnce sync.Once
	revoked := func(err error) {
		revokedOnce.Do(func() { g.revocations.revoked(identity.Execution, err) })
	}
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
				if err := g.revalidate(ctx, identity); err != nil {
					// A session that reached its lifetime is not revoked.
					if ctx.Err() == nil {
						revoked(err)
					}
					cancel()
					_ = listener.Close()
					return
				}
			}
		}
	}()
	defer func() { cancel(); _ = listener.Close(); <-monitorDone }()
	// Requests end before the connection so a handler can still reply, for
	// example a 502 for a model call cut short. Short test lifetimes keep a
	// proportional margin.
	requestDeadline := deadline.Add(-margin)
	server := &http.Server{
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			// A request that arrives after the request deadline has no time
			// left; ask the client to retry on a new connection.
			if !time.Now().Before(requestDeadline) {
				w.Header().Set("Connection", "close")
				http.Error(w, "gateway connection reached its lifetime; open a new connection and retry",
					http.StatusServiceUnavailable)
				return
			}
			call := &modelCallGrant{base: request.Context(), start: time.Now(),
				max: g.config.MaxModelCallDuration, session: life}
			requestCtx, cancelRequest := context.WithDeadline(
				context.WithValue(request.Context(), modelCallKey{}, call), requestDeadline)
			defer cancelRequest()
			g.serveInspected(w, request.WithContext(requestCtx), identity, host, port, revoked)
			if errors.Is(requestCtx.Err(), context.DeadlineExceeded) && !call.wasExtended() {
				// The harness sees an error reply or an ended stream, not why.
				g.logger.Info("Ended inspected request at connection lifetime",
					"execution", identity.Execution.UID, "host", host, "method", request.Method,
					"lifetime", g.config.SessionLifetime.String())
			}
		}),
	}
	// This server holds one connection, so disabling keep-alive at the request
	// deadline closes it when idle and marks later replies Connection: close.
	stopKeepAlive := time.AfterFunc(time.Until(requestDeadline), func() { server.SetKeepAlivesEnabled(false) })
	defer stopKeepAlive.Stop()
	_ = server.Serve(listener)
}

func (g *Gateway) serveInspected(
	w http.ResponseWriter, request *http.Request, identity *gateway.ExecutionGrant, host, port string,
	revoked func(error),
) {
	if err := g.revalidate(request.Context(), identity); err != nil {
		if request.Context().Err() == nil {
			revoked(err)
		}
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

// authFailureMaxCauses bounds the distinct causes an authFailureLog tracks in
// one window. Later causes are counted, not logged, until a window closes.
const authFailureMaxCauses = 32

// authFailureLog reports refused gateway identities. A refused token has no
// execution to group by, so it groups by cause instead: the first refusal
// with a cause in a window is logged with the client address, and one summary
// line counts the rest when the window closes. Lines carry the cause and the
// client address, never the token.
type authFailureLog struct {
	logger *slog.Logger
	window time.Duration

	mu       sync.Mutex
	bursts   map[string]int
	overflow int
}

func newAuthFailureLog(logger *slog.Logger) *authFailureLog {
	return &authFailureLog{logger: logger, window: executionLogWindow, bursts: map[string]int{}}
}

// record reports one refused identity with its cause.
func (l *authFailureLog) record(cause error, remote string) {
	reason := cause.Error()
	l.mu.Lock()
	if more, open := l.bursts[reason]; open {
		l.bursts[reason] = more + 1
		l.mu.Unlock()
		return
	}
	if len(l.bursts) >= authFailureMaxCauses {
		l.overflow++
		l.mu.Unlock()
		return
	}
	l.bursts[reason] = 0
	l.mu.Unlock()
	l.logger.Info("Rejected inspected connection with invalid gateway identity", "reason", reason, "remote", remote)
	time.AfterFunc(l.window, func() { l.flush(reason) })
}

func (l *authFailureLog) flush(reason string) {
	l.mu.Lock()
	more := l.bursts[reason]
	delete(l.bursts, reason)
	overflow := 0
	if len(l.bursts) == 0 {
		overflow, l.overflow = l.overflow, 0
	}
	l.mu.Unlock()
	if more > 0 {
		l.logger.Info("Rejected further inspected connections with invalid gateway identity", "count", more, "reason", reason)
	}
	if overflow > 0 {
		l.logger.Info("Rejected further inspected connections with invalid gateway identity", "count", overflow,
			"reason", "other causes")
	}
}
