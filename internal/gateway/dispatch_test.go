package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

func TestDispatcherRequiresContextIdentityAndStripsCredentials(t *testing.T) {
	called := false
	d := gateway.Dispatcher{"service.test:443": {Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Header.Get("Authorization") != "" || r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("credentials reached semantic handler")
		}
		w.WriteHeader(http.StatusNoContent)
	})}}
	req := httptest.NewRequest("GET", "https://service.test/path", nil)
	req.Header.Set("Authorization", "provider-secret")
	req.Header.Set("Proxy-Authorization", "run-token")
	req.Header.Set("Cookie", "session-secret")
	denied := httptest.NewRecorder()
	d.ServeHTTP(denied, req)
	if called || denied.Code != http.StatusUnauthorized {
		t.Fatal("accepted request without context identity")
	}
	req = req.WithContext(gateway.WithIdentity(req.Context(), &gateway.RunIdentity{Run: &api.AgentRun{}, Policy: &api.AgentPolicy{}}))
	allowed := httptest.NewRecorder()
	d.ServeHTTP(allowed, req)
	if !called || allowed.Code != http.StatusNoContent {
		t.Fatal("authenticated request not dispatched")
	}
}

func TestDispatcherDeliversFirstChunkBeforeHandlerCompletes(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	d := gateway.Dispatcher{"service.test:443": {Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		<-release
	})}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = "service.test"
		d.ServeHTTP(w, r.WithContext(gateway.WithIdentity(r.Context(), &gateway.RunIdentity{Run: &api.AgentRun{}, Policy: &api.AgentPolicy{}})))
	}))
	defer server.Close()
	got := make(chan error, 1)
	go func() {
		resp, err := http.Get(server.URL)
		if err == nil {
			defer func() { _ = resp.Body.Close() }()
			b := make([]byte, 6)
			_, err = io.ReadFull(resp.Body, b)
		}
		got <- err
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("dispatcher buffered streaming response")
	}
	// Release before httptest waits for the active handler.
	release <- struct{}{}
}
