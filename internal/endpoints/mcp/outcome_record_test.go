package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/runtime"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	remote "github.com/andrewmccall/sproozi/internal/endpoints/mcp"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

var updateRecords = flag.Bool("update-records", false,
	"rewrite testdata/outcomes.txt from the current code")

// compareRecorded compares lines with the recorded file, or rewrites it under
// -update-records.
func compareRecorded(t *testing.T, file string, lines []string) {
	t.Helper()
	got := strings.Join(lines, "\n") + "\n"
	if *updateRecords {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	wantLines := strings.Split(strings.TrimSuffix(string(want), "\n"), "\n")
	if len(wantLines) != len(lines) {
		t.Fatalf("enumerated %d lines, recorded %d", len(lines), len(wantLines))
	}
	for i := range lines {
		if lines[i] != wantLines[i] {
			t.Errorf("outcome changed:\n got %s\nwant %s", lines[i], wantLines[i])
		}
	}
}

// recordedEvents keeps every audit event a route or handler writes.
type recordedEvents struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *recordedEvents) Log(e audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recordedEvents) taken() []audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

// outcomeLine is one recorded case: status, the headers that matter, the
// body, the ledger before and after, and each audit event as sorted-key JSON
// without its timestamp.
func outcomeLine(t *testing.T, name string, response *httptest.ResponseRecorder, ledger string, events []audit.Event) string {
	t.Helper()
	var headers []string
	for _, h := range []string{"Content-Type", "X-Content-Type-Options", "Cache-Control", "Connection"} {
		if v := response.Header().Get(h); v != "" {
			headers = append(headers, strings.ToLower(h)+"="+v)
		}
	}
	recorded := make([]string, 0, len(events))
	for _, e := range events {
		recorded = append(recorded, eventJSON(t, e))
	}
	if len(recorded) == 0 {
		recorded = append(recorded, "none")
	}
	return strings.Join([]string{name, strconv.Itoa(response.Code), strings.Join(headers, " "),
		strconv.Quote(response.Body.String()), ledger, strings.Join(recorded, " ; ")}, " | ")
}

func eventJSON(t *testing.T, e audit.Event) string {
	t.Helper()
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var fields map[string]any
	if err := decoder.Decode(&fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "timestamp")
	sorted, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(sorted)
}

// failingLedger refuses every read and write, as an unavailable store does.
type failingLedger struct{}

var errLedgerUnavailable = errors.New("ledger store unavailable")

func (failingLedger) Initialize(context.Context, budget.AccountKey) error {
	return errLedgerUnavailable
}
func (failingLedger) Reserve(context.Context, budget.AccountKey, budget.UsageReservation) (*budget.LedgerReservation, error) {
	return nil, errLedgerUnavailable
}
func (failingLedger) Settle(context.Context, *budget.LedgerReservation, budget.TrustedUsage) error {
	return errLedgerUnavailable
}
func (failingLedger) Release(context.Context, *budget.LedgerReservation) error {
	return errLedgerUnavailable
}
func (failingLedger) State(context.Context, budget.AccountKey) (budget.State, error) {
	return budget.State{}, errLedgerUnavailable
}
func (failingLedger) Seal(context.Context, budget.AccountKey) (budget.TrustedUsage, error) {
	return budget.TrustedUsage{}, errLedgerUnavailable
}

func ledgerReading(tracker *budget.Tracker, key string) string {
	if tracker == nil || key == "" {
		return "-"
	}
	state, err := tracker.State(key)
	if err != nil {
		return "error"
	}
	return fmt.Sprintf("used=%d reserved=%d cost=%d", state.UnitsUsed, state.ReservedUnits, state.CostMicros)
}

// recordedMCPUpstream serves a docs provider with a search tool and a tool
// that always fails, and returns its registry and an HTTP client that trusts it.
func recordedMCPUpstream(t *testing.T) (*remote.Registry, *http.Client) {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: docsName, Version: "test"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: searchTool, Description: "Search a collection"},
		func(_ context.Context, _ *sdk.CallToolRequest, args struct {
			Collection string `json:"collection"`
			Query      string `json:"query"`
		}) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: args.Collection + ": guide for " + args.Query}}}, nil, nil
		})
	sdk.AddTool(server, &sdk.Tool{Name: "fail", Description: "Always fails"},
		func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "provider diagnostic"}}}, nil, nil
		})
	endpoint := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{JSONResponse: true})
	upstream := httptest.NewTLSServer(endpoint)
	t.Cleanup(upstream.Close)
	down := httptest.NewTLSServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()
	dir := t.TempDir()
	token := filepath.Join(dir, "docs.token")
	if err := os.WriteFile(token, []byte("docs-credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"servers": map[string]any{
		docsName: map[string]string{"url": upstream.URL + "/mcp", "bearerTokenFile": token},
		"down":   map[string]string{"url": downURL + "/mcp"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := remote.LoadRegistry(data, dir)
	if err != nil {
		t.Fatal(err)
	}
	return registry, upstream.Client()
}

// TestNamedMCPOutcomesAreRecorded records what a named MCP connection
// replies, charges and audits on the gateway's own route, which declares no
// capability, for each way a request ends.
func TestNamedMCPOutcomesAreRecorded(t *testing.T) {
	const docs api.CapabilityKind = "mcp.docs"
	const down api.CapabilityKind = "mcp.down"
	const authority = "gateway.test:443"
	registry, upstreamClient := recordedMCPUpstream(t)
	scope := func(tools ...string) api.MCPServerScope {
		s := api.MCPServerScope{Tools: map[string]api.MCPToolScope{}}
		for _, tool := range tools {
			rule := api.MCPToolScope{}
			if tool == searchTool {
				rule.Arguments = &runtime.RawExtension{Raw: []byte(`{"type":"object","required":["collection"],"properties":{"collection":{"const":"operations"}}}`)}
			}
			s.Tools[tool] = rule
		}
		return s
	}
	grant := func(change func(*gateway.ExecutionGrant)) *gateway.ExecutionGrant {
		g := &gateway.ExecutionGrant{Execution: gateway.Reference{Backend: gateway.BackendKubernetes, Namespace: "agents", Name: "run", UID: "test-execution"},
			Authority: gateway.Reference{Backend: gateway.BackendKubernetes, Namespace: "agents", Name: "runner", UID: "sa-uid"},
			Policy: gateway.PolicySnapshot{Name: "test-policy", Generation: 7, Spec: api.AgentPolicySpec{
				AllowedCapabilities: []api.CapabilityKind{docs, down},
				MCPServers:          map[string]api.MCPServerScope{docsName: scope(searchTool, "fail"), "down": scope(searchTool)},
				Budgets:             map[api.CapabilityKind]api.EndpointBudget{docs: {MaxUnits: 5}},
			}},
			Template:     gateway.TemplateSnapshot{Name: "test-template"},
			Capabilities: []api.CapabilityKind{docs, down}}
		if change != nil {
			change(g)
		}
		return g
	}
	call := func(id int, method, params string) string {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q`, id, method)
		if params != "" {
			body += `,"params":` + params
		}
		return body + "}"
	}
	initialize := call(1, "initialize", `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"recorder","version":"1"}}`)
	search := func(collection string) string {
		return call(3, "tools/call", `{"name":"search","arguments":{"collection":"`+collection+`","query":"DNS"}}`)
	}
	const docsURL = "https://gateway.test/mcp/docs"
	key := "test-execution/" + string(docs)
	cases := []struct {
		name, method, target, body string
		header                     map[string]string
		change                     func(*gateway.ExecutionGrant)
		noTracker, failing         bool
		spent                      int64
	}{
		{name: "query refused", method: http.MethodPost, target: docsURL + "?x=1", body: initialize},
		{name: "browser origin refused", method: http.MethodPost, target: docsURL, body: initialize, header: map[string]string{"Origin": "https://browser.example"}},
		{name: "invalid server name", method: http.MethodPost, target: "https://gateway.test/mcp/Not_A_Name", body: initialize},
		{name: "unregistered server", method: http.MethodPost, target: "https://gateway.test/mcp/other", body: initialize},
		{name: "capability denied", method: http.MethodPost, target: docsURL, body: initialize,
			change: func(g *gateway.ExecutionGrant) { g.Capabilities = []api.CapabilityKind{down} }},
		{name: "POST required", method: http.MethodGet, target: docsURL},
		{name: "invalid tool scope", method: http.MethodPost, target: docsURL, body: initialize,
			change: func(g *gateway.ExecutionGrant) { g.Policy.Spec.MCPServers[docsName] = api.MCPServerScope{} }},
		{name: "oversized body", method: http.MethodPost, target: docsURL, body: strings.Repeat(" ", 65<<10)},
		{name: "invalid JSON", method: http.MethodPost, target: docsURL, body: `{"jsonrpc":`},
		{name: "unsupported envelope", method: http.MethodPost, target: docsURL, body: call(2, "resources/list", "")},
		{name: "tool denied by scope", method: http.MethodPost, target: docsURL, body: call(3, "tools/call", `{"name":"delete_everything","arguments":{}}`)},
		{name: "allocation denied", method: http.MethodPost, target: docsURL, body: initialize,
			change: func(g *gateway.ExecutionGrant) {
				g.Managed = true
				g.Allocations = map[api.CapabilityKind]gateway.Allocation{}
			}},
		{name: "monetary cap", method: http.MethodPost, target: docsURL, body: initialize,
			change: func(g *gateway.ExecutionGrant) {
				g.Policy.Spec.Budgets = map[api.CapabilityKind]api.EndpointBudget{docs: {MaxUnits: 5, MaxCostMicros: 10}}
			}},
		{name: "budget configured without a tracker", method: http.MethodPost, target: docsURL, body: initialize, noTracker: true},
		{name: "managed ledger unavailable", method: http.MethodPost, target: docsURL, body: initialize, failing: true,
			change: func(g *gateway.ExecutionGrant) {
				g.Managed = true
				g.Allocations = map[api.CapabilityKind]gateway.Allocation{
					docs: {AccountKey: "managed/mcp.docs", Ceiling: api.EndpointBudget{MaxUnits: 5}},
					down: {AccountKey: "managed/mcp.down", Ceiling: api.EndpointBudget{MaxUnits: 5}},
				}
			}},
		{name: "upstream connection failed", method: http.MethodPost, target: "https://gateway.test/mcp/down", body: initialize},
		{name: "approved tool missing upstream", method: http.MethodPost, target: docsURL, body: initialize,
			change: func(g *gateway.ExecutionGrant) { g.Policy.Spec.MCPServers[docsName] = scope(searchTool, "absent") }},
		{name: "initialize", method: http.MethodPost, target: docsURL, body: initialize},
		{name: "tools/list", method: http.MethodPost, target: docsURL, body: call(2, "tools/list", "")},
		{name: "ping", method: http.MethodPost, target: docsURL, body: call(4, "ping", "")},
		{name: "notifications/initialized", method: http.MethodPost, target: docsURL, body: `{"jsonrpc":"2.0","method":"notifications/initialized"}`},
		{name: "tools/call served", method: http.MethodPost, target: docsURL, body: search("operations")},
		{name: "tools/call arguments denied", method: http.MethodPost, target: docsURL, body: search("private")},
		{name: "tools/call budget exhausted", method: http.MethodPost, target: docsURL, body: search("operations"), spent: 5},
		{name: "tools/call upstream tool failed", method: http.MethodPost, target: docsURL, body: call(3, "tools/call", `{"name":"fail","arguments":{}}`)},
	}
	lines := make([]string, 0, len(cases))
	for _, tc := range cases {
		events := &recordedEvents{}
		tracker := budget.NewTracker()
		if tc.failing {
			tracker = budget.NewTrackerWithLedger(failingLedger{})
		}
		if tc.spent > 0 {
			if err := tracker.Meter(key, api.EndpointBudget{MaxUnits: tc.spent}).Spend(context.Background(), tc.spent, 0); err != nil {
				t.Fatal(err)
			}
		}
		handlerBudgets := tracker
		if tc.noTracker {
			handlerBudgets = nil
		}
		handler := remote.NewHandler(remote.HandlerConfig{Registry: registry, HTTPClient: upstreamClient, Budgets: handlerBudgets, AuditLogger: events})
		routes := gateway.Dispatcher{authority: {Tier: audit.TierProtocol, Handler: handler, Budgets: tracker, AuditLogger: events}}
		ledgerKey := key
		if tc.failing {
			ledgerKey = ""
		}
		before := ledgerReading(tracker, ledgerKey)
		var body io.Reader
		if tc.body != "" {
			body = strings.NewReader(tc.body)
		}
		req := httptest.NewRequest(tc.method, tc.target, body)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for k, v := range tc.header {
			req.Header.Set(k, v)
		}
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, req.WithContext(gateway.WithIdentity(req.Context(), grant(tc.change))))
		lines = append(lines, outcomeLine(t, tc.name, response, before+" -> "+ledgerReading(tracker, ledgerKey), events.taken()))
	}
	compareRecorded(t, filepath.Join("testdata", "outcomes.txt"), lines)
}
