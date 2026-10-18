package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestConnectDeadlineCoversDiscoveryAndLegacyInitialization proves that the
// caller's context bounds both discovery and the legacy initialize fallback.
// The context ends only once initialize is in flight, so the result does not
// depend on how quickly either exchange runs.
func TestConnectDeadlineCoversDiscoveryAndLegacyInitialization(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "legacy-tools", Version: "test"}, nil)
	endpoint := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{JSONResponse: true})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var exchanges atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			endpoint.ServeHTTP(w, r)
			return
		}
		var request struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		switch request.Method {
		case discoverMethod:
			exchanges.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID,
				"error": map[string]any{"code": -32601, "message": "Method not found"}})
		case "initialize":
			exchanges.Add(1)
			// End the caller's context while initialize is outstanding. Setup
			// must abandon this request rather than wait for an HTTP timeout.
			cancel()
			<-r.Context().Done()
		default:
			// Only cancellation notifications can complete after initialize ends.
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(HandlerConfig{})
	type outcome struct {
		session *sdk.ClientSession
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		session, err := h.connect(ctx, registration{endpoint: upstreamURL})
		done <- outcome{session, err}
	}()
	select {
	case got := <-done:
		if got.session != nil {
			_ = got.session.Close()
		}
		if got.err == nil || !errors.Is(ctx.Err(), context.Canceled) || exchanges.Load() != 2 {
			t.Fatalf("aggregate setup result: session=%v err=%v context=%v exchanges=%d", got.session, got.err, ctx.Err(), exchanges.Load())
		}
	case <-time.After(20 * time.Second):
		// Only reached when initialize ignores the caller's context and waits
		// for the client's own 30-second HTTP timeout.
		t.Fatal("setup outlived its context by an independent HTTP timeout")
	}
}
