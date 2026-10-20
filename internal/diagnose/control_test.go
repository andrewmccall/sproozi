package diagnose_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/diagnose"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

// asExecution stands in for proxytransport: the header is test-only and selects
// which authenticated execution the request belongs to.
func asExecution(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if uid := r.Header.Get("X-Test-Execution"); uid != "" {
			r = r.WithContext(gateway.WithIdentity(r.Context(), &gateway.ExecutionGrant{
				Execution: gateway.Reference{Backend: gateway.BackendKubernetes, Namespace: "agents", Name: uid, UID: uid}}))
		}
		next.ServeHTTP(w, r)
	})
}

type headerTransport struct{ execution string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Test-Execution", h.execution)
	return http.DefaultTransport.RoundTrip(r)
}

func diagnoseAs(t *testing.T, url, execution string, args map[string]any) diagnose.Result {
	t.Helper()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v1"}, nil).Connect(t.Context(),
		&mcp.StreamableClientTransport{Endpoint: url + "/mcp/sproozi", HTTPClient: &http.Client{Transport: headerTransport{execution}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "diagnose" {
		t.Fatalf("tools = %+v, %v", tools, err)
	}
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "diagnose", Arguments: args})
	if err != nil || result.IsError {
		t.Fatalf("diagnose = %+v, %v", result, err)
	}
	var got diagnose.Result
	if err := json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestDiagnoseReturnsOnlyTheCallersMatchingDenials(t *testing.T) {
	journal := diagnose.NewJournal(nil, diagnose.Limits{})
	now := time.Now().UTC()
	journal.Log(audit.Event{Timestamp: now, RunUID: runA, Operation: "CONNECT docs.example.com:443",
		Capability: egress, SemanticLevel: audit.TierDestination, DenyReason: "destination docs.example.com:443 is not enabled at this gateway"})
	journal.Log(audit.Event{Timestamp: now, RunUID: runB, Operation: "CONNECT secret.example.com:443", DenyReason: "denied"})
	journal.Log(audit.Event{Timestamp: now, RunUID: runA, Operation: "GET api.github.com:443", Resource: "org/repo", DenyReason: "repository denied"})
	server := httptest.NewServer(asExecution(diagnose.NewControl(journal, http.NotFoundHandler())))
	defer server.Close()

	got := diagnoseAs(t, server.URL, runA, map[string]any{"destination": "docs.example.com"})
	if got.Status != diagnose.StatusDenied || len(got.Decisions) != 1 {
		t.Fatalf("result = %+v", got)
	}
	d := got.Decisions[0]
	if d.Source != "gateway" || d.Capability != egress || d.Operation != "CONNECT docs.example.com:443" || d.Reason == "" || d.DecisionID == "" {
		t.Fatalf("decision = %+v", d)
	}
	byID := diagnoseAs(t, server.URL, runA, map[string]any{"decision_id": d.DecisionID})
	if byID.Status != diagnose.StatusDenied || len(byID.Decisions) != 1 || byID.Decisions[0].DecisionID != d.DecisionID {
		t.Fatalf("by id = %+v", byID)
	}

	other := diagnoseAs(t, server.URL, runB, map[string]any{"decision_id": d.DecisionID})
	if other.Status != diagnose.StatusNotObserved || len(other.Decisions) != 0 {
		t.Fatalf("run-b read run-a's decision: %+v", other)
	}
	all := diagnoseAs(t, server.URL, runB, nil)
	if len(all.Decisions) != 1 || all.Decisions[0].Operation != "CONNECT secret.example.com:443" {
		t.Fatalf("run-b decisions = %+v", all)
	}
}

// A request blocked before the gateway leaves no decision; diagnose must say
// so rather than invent a denial.
func TestDiagnoseReportsNotObservedWithoutInventingADenial(t *testing.T) {
	server := httptest.NewServer(asExecution(diagnose.NewControl(diagnose.NewJournal(nil, diagnose.Limits{}), http.NotFoundHandler())))
	defer server.Close()

	got := diagnoseAs(t, server.URL, runA, map[string]any{"destination": "10.0.0.1", "lookback_seconds": 30})
	if got.Status != diagnose.StatusNotObserved || len(got.Decisions) != 0 || got.Note == "" {
		t.Fatalf("result = %+v", got)
	}
}

func TestControlRequiresIdentityAndDelegatesOtherPaths(t *testing.T) {
	delegated := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { delegated = true })
	server := httptest.NewServer(asExecution(diagnose.NewControl(diagnose.NewJournal(nil, diagnose.Limits{}), next)))
	defer server.Close()

	resp, err := http.Post(server.URL+"/mcp/sproozi", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", resp.StatusCode)
	}
	resp, err = http.Post(server.URL+"/mcp/docs", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !delegated {
		t.Fatal("named MCP path was not delegated")
	}
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/mcp/sproozi", nil)
	request.Header.Set("Origin", "https://evil.example")
	request.Header.Set("X-Test-Execution", runA)
	resp, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("browser origin status = %d", resp.StatusCode)
	}
}
