package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestConnectDeadlineCoversDiscoveryAndLegacyInitialization(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "legacy-tools", Version: "test"}, nil)
	endpoint := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{JSONResponse: true})
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
		if request.Method == discoverMethod || request.Method == "initialize" {
			exchanges.Add(1)
			select {
			case <-time.After(40 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
		if request.Method == discoverMethod {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID,
				"error": map[string]any{"code": -32601, "message": "Method not found"}})
			return
		}
		// Only cancellation notifications can complete after the initialize deadline.
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(HandlerConfig{})
	ctx, cancel := context.WithTimeout(t.Context(), 65*time.Millisecond)
	defer cancel()
	started := time.Now()
	session, err := h.connect(ctx, registration{endpoint: upstreamURL})
	if session != nil {
		_ = session.Close()
	}
	if err == nil || ctx.Err() != context.DeadlineExceeded || exchanges.Load() != 2 {
		t.Fatalf("aggregate setup result: session=%v err=%v context=%v exchanges=%d", session, err, ctx.Err(), exchanges.Load())
	}
	if time.Since(started) > time.Second {
		t.Fatal("setup outlived its deadline by an independent HTTP timeout")
	}
}
