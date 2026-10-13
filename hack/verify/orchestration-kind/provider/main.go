// Command provider is a deterministic, credential-separated acceptance fixture.
//
//nolint:goconst // Keep literal JSON protocol and evidence fields reviewable.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	claudeClient = "claude-code"
	caseField    = "case"
)

const answer = "SPROOZI_KIND_WORKER_COMPLETE"
const providerKey = "sproozi-kind-worker-provider-credential"
const coordinatorKey = "sproozi-kind-coordinator-provider-credential"

type fixture struct {
	mu                       sync.Mutex
	requests, tools          int
	violations               []string
	tool, receivedToolResult bool
	records                  []map[string]any
}

func main() {
	f := &fixture{tool: true}
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "verify",
		Description: "Return the exact acceptance marker; call once before answering"}, func(_ context.Context,
		_ *mcp.CallToolRequest, input map[string]any) (*mcp.CallToolResult, any, error) {
		f.mu.Lock()
		f.tools++
		caseID, _ := input[caseField].(string)
		f.records = append(f.records, map[string]any{"kind": "tool", caseField: caseID})
		f.mu.Unlock()
		marker := answer + " " + caseID + " SPROOZI_CASE=" + caseID
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: marker}}}, nil, nil
	})
	upstream := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{JSONResponse: true, Stateless: true, DisableLocalhostProtection: true})
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" ||
			r.Header.Get("Authorization") != "Bearer sproozi-kind-mcp-provider-credential" {
			http.Error(w, "provider credential required", http.StatusUnauthorized)
			return
		}
		upstream.ServeHTTP(w, r)
	})
	mux.HandleFunc("/evidence", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"requests": f.requests, "tools": f.tools,
			"receivedToolResult": f.receivedToolResult, "violations": f.violations, "records": f.records})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /v1/models", f.catalog)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		coordinator := subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")),
			[]byte("Bearer "+coordinatorKey)) == 1
		if coordinator {
			f.coordinator(w, r)
			return
		}
		selected := "openai"
		if strings.Contains(r.URL.Path, "messages") {
			selected = claudeClient
		}
		f.model(w, r, selected)
	})
	s := &http.Server{Addr: ":8443", Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := s.ListenAndServeTLS(os.Getenv("TLS_CERT_FILE"), os.Getenv("TLS_KEY_FILE")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Native Hermes also probes a custom provider's catalog without credentials.
// Reject that discovery request without confusing it with gateway inference;
// catalog responses never contain a tool result or an execution success marker.
func (f *fixture) catalog(w http.ResponseWriter, r *http.Request) {
	credential := r.Header.Get("Authorization")
	authenticated := subtle.ConstantTimeCompare([]byte(credential), []byte("Bearer "+providerKey)) == 1 ||
		subtle.ConstantTimeCompare([]byte(credential), []byte("Bearer "+coordinatorKey)) == 1
	f.mu.Lock()
	f.records = append(f.records, map[string]any{"kind": "catalog", "path": r.URL.Path,
		"method": r.Method, "authenticated": authenticated})
	if r.Header.Get("Proxy-Authorization") != "" {
		f.violations = append(f.violations, "proxy authentication header reached catalog")
	}
	f.mu.Unlock()
	if !authenticated {
		http.Error(w, "provider credential required", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{
		map[string]any{"id": "fixture-model", "object": "model", "context_length": 256000}}})
}
