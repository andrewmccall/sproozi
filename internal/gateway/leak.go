package gateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultCredentialLifetime bounds how long the gateway holds a run's
// provider credential when its policy sets no maximum execution duration.
const DefaultCredentialLifetime = 24 * time.Hour

// minWatchedCredential keeps short values, such as placeholders, from
// refusing ordinary text.
const minWatchedCredential = 16

// ErrCredentialLeak ends a request body that carries the run's own provider
// credential to a route other than the one it was sent to.
var ErrCredentialLeak = errors.New("request carries the run's provider credential")

// LeakGuard wraps a Dispatcher. On a ProviderCredential route it remembers
// the credential the workload sent, in memory only, until the run's maximum
// duration has passed. On every other route it refuses a request whose URL,
// headers or body contain one of those credentials (ADR 0047). It sees only
// inspected traffic, and cannot recognize an encoded or split credential.
type LeakGuard struct {
	routes Dispatcher
	now    func() time.Time
	mu     sync.Mutex
	runs   map[string]*watchedRun
}

type watchedRun struct {
	expires time.Time
	// authorities maps each credential to the route it was sent to.
	authorities map[string]string
}

// NewLeakGuard wraps routes; a nil now uses the wall clock.
func NewLeakGuard(routes Dispatcher, now func() time.Time) *LeakGuard {
	if now == nil {
		now = time.Now
	}
	return &LeakGuard{routes: routes, now: now, runs: map[string]*watchedRun{}}
}

// Allows reports whether the wrapped Dispatcher serves authority.
func (g *LeakGuard) Allows(authority string) bool { return g.routes.Allows(authority) }

// ForwardsCredential reports whether the route serving path on authority
// forwards the harness's own provider credential.
func (g *LeakGuard) ForwardsCredential(authority, requestPath string) bool {
	route, _, ok := g.routes.Select(authority, requestPath)
	return ok && route.ProviderCredential
}

// Watched reports how many credentials the guard holds for an execution.
func (g *LeakGuard) Watched(uid string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if run := g.runs[uid]; run != nil && g.now().Before(run.expires) {
		return len(run.authorities)
	}
	return 0
}

func (g *LeakGuard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	identity, ok := IdentityFromContext(r.Context())
	if !ok {
		g.routes.ServeHTTP(w, r)
		return
	}
	// A credential belongs to the route it was sent to, so the same host's
	// other routes, such as a pull request on api.github.com, refuse it.
	route, key, _ := g.routes.Select(r.Host, r.URL.Path)
	if route.ProviderCredential {
		g.observe(identity, key, r.Header)
		g.routes.ServeHTTP(w, r)
		return
	}
	secrets := g.secrets(identity.Execution.UID, key)
	if len(secrets) == 0 {
		g.routes.ServeHTTP(w, r)
		return
	}
	if containsAny(r.URL.String(), secrets) || headersContain(r.Header, secrets) {
		writeDenial(w, http.StatusForbidden, "permission_error", "credential_leak_denied",
			"the request to "+r.Host+" carries this run's own provider credential; "+
				"the gateway sends a provider credential only to its provider")
		return
	}
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = newLeakReader(r.Body, secrets)
	}
	g.routes.ServeHTTP(w, r)
}

func (g *LeakGuard) observe(identity *ExecutionGrant, authority string, header http.Header) {
	credential := strings.TrimSpace(header.Get("X-Api-Key"))
	if bearer, ok := strings.CutPrefix(header.Get("Authorization"), "Bearer "); ok && credential == "" {
		credential = strings.TrimSpace(bearer)
	}
	if len(credential) < minWatchedCredential {
		return
	}
	lifetime := identity.Policy.Spec.MaxExecutionDuration.Duration
	if lifetime <= 0 {
		lifetime = DefaultCredentialLifetime
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	for uid, run := range g.runs {
		if !now.Before(run.expires) {
			delete(g.runs, uid)
		}
	}
	run := g.runs[identity.Execution.UID]
	if run == nil {
		run = &watchedRun{authorities: map[string]string{}}
		g.runs[identity.Execution.UID] = run
	}
	run.expires = now.Add(lifetime)
	run.authorities[credential] = authority
}

// secrets returns the execution's credentials that belong to other routes.
func (g *LeakGuard) secrets(uid, authority string) [][]byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	run := g.runs[uid]
	if run == nil || !g.now().Before(run.expires) {
		return nil
	}
	var out [][]byte
	for credential, owner := range run.authorities {
		if owner != authority {
			out = append(out, []byte(credential))
		}
	}
	return out
}

func headersContain(header http.Header, secrets [][]byte) bool {
	for _, values := range header {
		for _, value := range values {
			if containsAny(value, secrets) {
				return true
			}
		}
	}
	return false
}

func containsAny(text string, secrets [][]byte) bool {
	for _, secret := range secrets {
		if strings.Contains(text, string(secret)) {
			return true
		}
	}
	return false
}

// leakReader forwards a body while holding back the last len(longest)-1
// bytes, so a complete credential is never released before it is checked.
type leakReader struct {
	src      io.ReadCloser
	secrets  [][]byte
	hold     int
	pending  []byte
	released []byte
	err      error
}

func newLeakReader(src io.ReadCloser, secrets [][]byte) *leakReader {
	hold := 0
	for _, secret := range secrets {
		hold = max(hold, len(secret)-1)
	}
	return &leakReader{src: src, secrets: secrets, hold: hold}
}

func (l *leakReader) Read(p []byte) (int, error) {
	for len(l.released) == 0 {
		if l.err != nil {
			if errors.Is(l.err, io.EOF) && len(l.pending) > 0 {
				l.released, l.pending = l.pending, nil
				break
			}
			return 0, l.err
		}
		chunk := make([]byte, 32<<10)
		n, err := l.src.Read(chunk)
		l.pending = append(l.pending, chunk[:n]...)
		for _, secret := range l.secrets {
			if bytes.Contains(l.pending, secret) {
				l.pending, l.err = nil, ErrCredentialLeak
				return 0, l.err
			}
		}
		if err != nil {
			l.err = err
			continue
		}
		if release := len(l.pending) - l.hold; release > 0 {
			l.released = append(l.released, l.pending[:release]...)
			l.pending = append([]byte(nil), l.pending[release:]...)
		}
	}
	n := copy(p, l.released)
	l.released = l.released[n:]
	return n, nil
}

func (l *leakReader) Close() error { return l.src.Close() }
