package modelauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// expiredStore holds the fixture sign-in with an access token past its
// refresh window, so the next model request refreshes it.
func expiredStore() *memoryStore {
	store := &memoryStore{session: fixtureSession()}
	store.session.SavedAt = time.Now().Add(-2 * time.Hour)
	return store
}

// fakeClock is a clock the test moves; nothing in the package sleeps on it.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// withClock gives auth a fake clock that starts now.
func withClock(auth *ChatGPT) *fakeClock {
	clock := &fakeClock{t: time.Now()}
	auth.now = clock.Now
	return clock
}

func authorizeOnce(auth *ChatGPT) (string, error) {
	req := httptest.NewRequest(http.MethodPost, Resource+"/responses", nil)
	err := auth.Authorize(req)
	return req.Header.Get("Authorization"), err
}

// leaks reports whether text carries any fixture token material.
func leaks(text string) bool {
	return strings.Contains(text, "fixture-") || strings.Contains(text, "provider-secret")
}

// failingEndpoint answers every refresh with one kind of failure. The body
// always carries token-like text so a leak into an error is visible.
func failingEndpoint(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// TestChatGPTRefreshFailureOutcomes records, for each kind of refresh
// failure, what three model requests in a row see and how often they reach
// the provider's token endpoint. Guards: auth.go reloadOrRefresh and Exchange.
func TestChatGPTRefreshFailureOutcomes(t *testing.T) {
	const grantRefused = `{"error":"invalid_grant","error_description":"provider-secret fixture-refresh was revoked"}`
	for _, tc := range []struct {
		name string
		// handler is nil for a network error: nothing listens.
		handler http.HandlerFunc
		// slow makes the endpoint outlast the refresh client's timeout.
		slow bool
		// wantCalls is how many refresh requests three model requests send.
		wantCalls int32
		wantErr   string
		// refused: an administrator must sign in again; otherwise a hold.
		refused bool
	}{
		{name: "network error", wantCalls: 0, wantErr: "the ChatGPT sign-in could not be renewed (the token endpoint was unreachable or did not answer in time); the gateway tries again 10s after the failure"},
		{name: "timeout", slow: true, wantCalls: 1, wantErr: "the ChatGPT sign-in could not be renewed (the token endpoint was unreachable or did not answer in time); the gateway tries again 10s after the failure"},
		{name: "5xx", handler: failingEndpoint(http.StatusServiceUnavailable, `{"error":"provider-secret"}`),
			wantCalls: 1, wantErr: "the ChatGPT sign-in could not be renewed (token endpoint HTTP 503); the gateway tries again 10s after the failure"},
		{name: "429", handler: failingEndpoint(http.StatusTooManyRequests, `{"error":"provider-secret"}`),
			wantCalls: 1, wantErr: "the ChatGPT sign-in could not be renewed (token endpoint HTTP 429); the gateway tries again 10s after the failure"},
		{name: "400 invalid_grant", handler: failingEndpoint(http.StatusBadRequest, grantRefused),
			wantCalls: 1, wantErr: "the provider refused to renew the ChatGPT sign-in (token endpoint HTTP 400 invalid_grant); an administrator must sign in again", refused: true},
		{name: "401 invalid_grant", handler: failingEndpoint(http.StatusUnauthorized, grantRefused),
			wantCalls: 1, wantErr: "the provider refused to renew the ChatGPT sign-in (token endpoint HTTP 401 invalid_grant); an administrator must sign in again", refused: true},
		{name: "malformed response", handler: failingEndpoint(http.StatusOK, `{"access_token":"provider-secret"`),
			wantCalls: 1, wantErr: "the ChatGPT sign-in could not be renewed (the token endpoint returned an unreadable reply); the gateway tries again 10s after the failure"},
		{name: "no new refresh token", handler: failingEndpoint(http.StatusOK,
			`{"access_token":"provider-secret","token_type":"Bearer","expires_in":3600}`),
			wantCalls: 1, wantErr: "the ChatGPT sign-in could not be renewed (the renewed credential was incomplete); the gateway tries again 10s after the failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			client := &http.Client{Timeout: 5 * time.Second}
			endpoint := "http://127.0.0.1:1" // nothing listens on port 1
			if tc.handler != nil || tc.slow {
				stop := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if tc.slow {
						select {
						case <-r.Context().Done():
						case <-stop:
						}
						return
					}
					tc.handler(w, r)
				}))
				defer server.Close()
				defer close(stop)
				endpoint = server.URL
			}
			if tc.slow {
				client.Timeout = 50 * time.Millisecond
			}
			store := expiredStore()
			auth := NewChatGPT(store, client)
			auth.tokenEndpoint = endpoint
			clock := withClock(auth)
			for i := range 3 {
				header, err := authorizeOnce(auth)
				if err == nil || header != "" {
					t.Fatalf("request %d authorized after a failed refresh: %q", i, header)
				}
				if err.Error() != tc.wantErr {
					t.Fatalf("request %d error = %q, want %q", i, err, tc.wantErr)
				}
				if leaks(err.Error()) {
					t.Fatalf("request %d error leaks token material: %q", i, err)
				}
			}
			if got := calls.Load(); got != tc.wantCalls {
				t.Fatalf("refresh requests = %d, want %d", got, tc.wantCalls)
			}
			if store.session.RefreshToken != fixtureRefresh || store.session.AccessToken != fixtureAccess {
				t.Fatal("a failed refresh changed the stored sign-in")
			}
			// After the hold a transient failure is tried again; a refusal is not.
			clock.Advance(RefreshHold)
			_, _ = authorizeOnce(auth)
			wantAfter := tc.wantCalls
			if !tc.refused && tc.wantCalls > 0 {
				wantAfter++
			}
			if got := calls.Load(); got != wantAfter {
				t.Fatalf("refresh requests after the hold = %d, want %d", got, wantAfter)
			}
		})
	}
}

// TestChatGPTParallelRequestsDuringAnOutage records how many refresh requests
// reach the provider when many model requests arrive while the access token
// is expired and the provider is failing. Guards: the refresh lock in
// reloadOrRefresh.
func TestChatGPTParallelRequestsDuringAnOutage(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	auth := NewChatGPT(expiredStore(), server.Client())
	auth.tokenEndpoint = server.URL
	const requests = 10
	var wg sync.WaitGroup
	for range requests {
		wg.Go(func() {
			if header, err := authorizeOnce(auth); err == nil || header != "" {
				t.Errorf("authorized during an outage: %q", header)
			}
		})
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh requests = %d, want 1", got)
	}
}

// rotatingEndpoint is a fake token endpoint that, like the provider (ADR
// 0026), accepts each refresh token once and issues a new one.
type rotatingEndpoint struct {
	mu     sync.Mutex
	valid  map[string]bool
	issued int
	calls  atomic.Int32
	// rotated, when set, is closed once the endpoint has spent the first
	// token, and that reply waits for release. Both are set before serving.
	rotated chan struct{}
	release chan struct{}
}

func newRotatingEndpoint(refresh string) *rotatingEndpoint {
	return &rotatingEndpoint{valid: map[string]bool{refresh: true}}
}

func (e *rotatingEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.calls.Add(1)
	_ = r.ParseForm()
	e.mu.Lock()
	token := r.Form.Get(refreshTokenField)
	if !e.valid[token] {
		e.mu.Unlock()
		failingEndpoint(http.StatusBadRequest, `{"error":"invalid_grant"}`)(w, r)
		return
	}
	delete(e.valid, token)
	e.issued++
	next := "rotated-refresh-" + string(rune('0'+e.issued))
	e.valid[next] = true
	first := e.issued == 1
	e.mu.Unlock()
	if first && e.rotated != nil {
		close(e.rotated)
		<-e.release
	}
	_, _ = w.Write([]byte(`{"access_token":"rotated-access","refresh_token":"` + next +
		`","token_type":"Bearer","expires_in":3600}`))
}

// ctxStore refuses to load or write once its caller's context is done, as
// the Kubernetes client does.
type ctxStore struct{ *memoryStore }

func (s ctxStore) Load(ctx context.Context) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	return s.memoryStore.Load(ctx)
}

func (s ctxStore) Replace(ctx context.Context, before, after Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.memoryStore.Replace(ctx, before, after)
}

// TestChatGPTRefreshWhoseRequestLeavesKeepsTheRotatedToken: a model request
// whose harness leaves while the provider is rotating the refresh token.
// Guards: the context a refresh and its save run under.
func TestChatGPTRefreshWhoseRequestLeavesKeepsTheRotatedToken(t *testing.T) {
	rotated, release := make(chan struct{}), make(chan struct{})
	endpoint := newRotatingEndpoint(fixtureRefresh)
	endpoint.rotated, endpoint.release = rotated, release
	server := httptest.NewServer(endpoint)
	defer server.Close()
	store := expiredStore()
	auth := NewChatGPT(ctxStore{store}, server.Client())
	auth.tokenEndpoint = server.URL

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, Resource+"/responses", nil).WithContext(ctx)
		done <- auth.Authorize(req)
	}()
	<-rotated // the provider has spent fixtureRefresh
	cancel()  // the harness leaves before the reply arrives
	// Let the reply go only after the request has given up on it, as a reply
	// that crosses a disconnect does.
	select {
	case <-done:
		close(release)
	case <-time.After(200 * time.Millisecond):
		close(release)
		<-done
	}

	// The refresh and its save outlived the request: the stored sign-in holds
	// the rotated token, and the next request uses it without a refresh.
	if store.session.RefreshToken != "rotated-refresh-1" {
		t.Fatalf("stored refresh token = %q, want the rotated one", store.session.RefreshToken)
	}
	if header, err := authorizeOnce(auth); err != nil || header != "Bearer rotated-access" {
		t.Fatalf("next request = %q, %v", header, err)
	}
	if got := endpoint.calls.Load(); got != 1 {
		t.Fatalf("refresh requests = %d, want 1", got)
	}
}
