package work

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A stop whose request ends, because the CLI went away or its request timed
// out, goes on on the executor until the turn completes and the checkpoint is
// kept. The CLI's retry with the same operation then finds the session
// retained.
func TestOwnerStopOutlivesItsRequest(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	o := openOwner(t, c)
	v := startInteractive(t, o, interactiveRequest())
	home := strings.Repeat("h", 32)
	h, err := NewHandler(o, strings.Repeat("t", 32), homeToken(home))
	if err != nil {
		t.Fatal(err)
	}
	held, started := make(chan struct{}), make(chan struct{})
	p.mu.Lock()
	p.heldStop, p.stopStarted = held, started
	p.mu.Unlock()
	ctx, leave := context.WithCancel(t.Context())
	defer leave()
	op := uuid.NewString()
	r := httptest.NewRequest(http.MethodPost, "/work/stop", strings.NewReader(`{"id":"`+v.ID+`","operationId":"`+op+`"}`)).WithContext(ctx)
	r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13}
	r.Header.Set("Authorization", "Bearer "+home)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(w, r)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the stop did not start")
	}
	leave()
	select {
	case <-done:
		t.Fatalf("the executor gave up a stop whose request ended: %d %s", w.Code, w.Body.String())
	case <-time.After(200 * time.Millisecond):
	}
	close(held)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stop did not finish once its turn completed")
	}
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"phase":"retained"`) {
		t.Fatalf("the stop that outlived its request: %d %s", w.Code, w.Body.String())
	}
	again, err := o.Stop(t.Context(), v.ID, op)
	if err != nil || again.Phase != PhaseRetained {
		t.Fatalf("retry of the same stop: %+v %v", again, err)
	}
}
