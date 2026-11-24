package kubernetes_test

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
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/endpoints/kubernetes"
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

// recordTransport answers upstream requests in process.
type recordTransport func(*http.Request) (*http.Response, error)

func (f recordTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func recordResponse(r *http.Request, status int, header http.Header, body string) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

// TestKubernetesOutcomesAreRecorded records what a kubernetes.read route
// replies, charges and audits for each way a REST or /mcp request ends.
func TestKubernetesOutcomesAreRecorded(t *testing.T) {
	const capability = api.CapabilityKubernetesRead
	const podList = `{"kind":"PodList","items":[{"metadata":{"name":"example"}}]}`
	grant := func(units int64) *gateway.ExecutionGrant {
		g := &gateway.ExecutionGrant{Execution: gateway.Reference{Backend: gateway.BackendKubernetes, Namespace: "agents", Name: "run", UID: "test-execution"},
			Authority: gateway.Reference{Backend: gateway.BackendKubernetes, Namespace: "agents", Name: "runner", UID: "sa-uid"},
			Policy: gateway.PolicySnapshot{Name: "test-policy", Generation: 5, Spec: api.AgentPolicySpec{
				AllowedCapabilities: []api.CapabilityKind{capability},
				KubernetesRead: api.KubernetesReadScope{Namespaces: []string{"demo"},
					Resources: []api.KubernetesReadResource{api.KubernetesReadPods}},
			}},
			Template:     gateway.TemplateSnapshot{Name: "test-template"},
			Capabilities: []api.CapabilityKind{capability}}
		if units > 0 {
			g.Policy.Spec.Budgets = map[api.CapabilityKind]api.EndpointBudget{capability: {MaxUnits: units}}
		}
		return g
	}
	served := func(status int, body string) *http.Client {
		return &http.Client{Transport: recordTransport(func(r *http.Request) (*http.Response, error) {
			return recordResponse(r, status, http.Header{"Content-Type": {"application/json"}}, body), nil
		})}
	}
	failing := &http.Client{Transport: recordTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed")
	})}
	upstream, err := url.Parse("https://kube.test")
	if err != nil {
		t.Fatal(err)
	}
	const podsURL = "https://kubernetes.default.svc/api/v1/namespaces/demo/pods"
	const mcpURL = "https://kubernetes.default.svc/mcp"
	call := func(id int, method, params string) string {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q`, id, method)
		if params != "" {
			body += `,"params":` + params
		}
		return body + "}"
	}
	initialize := call(1, "initialize", `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"recorder","version":"1"}}`)
	listPods := func(namespace string) string {
		return call(3, "tools/call", `{"name":"kubernetes_list_pods","arguments":{"namespace":"`+namespace+`"}}`)
	}
	key := "test-execution/" + string(capability)
	cases := []struct {
		name, method, target, body string
		header                     map[string]string
		client                     *http.Client
		noUpstream                 bool
		units                      int64
		tracker                    *budget.Tracker
		spent                      bool
	}{
		{name: "rest/served", method: http.MethodGet, target: podsURL, client: served(200, podList), units: 5, tracker: budget.NewTracker()},
		{name: "rest/watch served", method: http.MethodGet, target: podsURL + "?watch=true&timeoutSeconds=5", client: served(200, `{"type":"ADDED"}`),
			units: 5, tracker: budget.NewTracker()},
		{name: "rest/discovery served", method: http.MethodGet, target: "https://kubernetes.default.svc/api/v1", client: served(200, `{"kind":"APIResourceList"}`),
			units: 5, tracker: budget.NewTracker()},
		{name: "rest/parse error: method", method: http.MethodPost, target: podsURL, client: served(200, podList)},
		{name: "rest/parse error: query bound", method: http.MethodGet, target: podsURL + "?limit=0", client: served(200, podList)},
		{name: "rest/parse error: unsupported query", method: http.MethodGet, target: podsURL + "?pretty=true", client: served(200, podList)},
		{name: "rest/policy: namespace", method: http.MethodGet, target: "https://kubernetes.default.svc/api/v1/namespaces/other/pods", client: served(200, podList)},
		{name: "rest/policy: resource", method: http.MethodGet, target: "https://kubernetes.default.svc/api/v1/namespaces/demo/secrets", client: served(200, podList)},
		{name: "rest/upstream not configured", method: http.MethodGet, target: podsURL, noUpstream: true},
		{name: "rest/budget exhausted", method: http.MethodGet, target: podsURL, client: served(200, podList), units: 1, tracker: budget.NewTracker(), spent: true},
		{name: "rest/watch budget exhausted", method: http.MethodGet, target: podsURL + "?watch=true&timeoutSeconds=5",
			client: served(200, `{"type":"ADDED"}`), units: 1, tracker: budget.NewTracker(), spent: true},
		{name: "rest/ledger unavailable", method: http.MethodGet, target: podsURL, client: served(200, podList), units: 1,
			tracker: budget.NewTrackerWithLedger(failingLedger{})},
		{name: "rest/upstream transport error", method: http.MethodGet, target: podsURL, client: failing, units: 5, tracker: budget.NewTracker()},
		{name: "rest/upstream non-2xx", method: http.MethodGet, target: podsURL, client: served(403, `{"kind":"Status"}`), units: 5, tracker: budget.NewTracker()},
		{name: "mcp/origin refused", method: http.MethodPost, target: mcpURL, body: initialize, header: map[string]string{"Origin": "https://attacker.test"},
			client: served(200, podList), units: 5, tracker: budget.NewTracker()},
		{name: "mcp/query refused", method: http.MethodPost, target: mcpURL + "?x=1", body: initialize, client: served(200, podList), units: 5, tracker: budget.NewTracker()},
		{name: "mcp/initialize", method: http.MethodPost, target: mcpURL, body: initialize, client: served(200, podList), units: 5, tracker: budget.NewTracker()},
		{name: "mcp/tools/list", method: http.MethodPost, target: mcpURL, body: call(2, "tools/list", ""), client: served(200, podList), units: 5, tracker: budget.NewTracker()},
		{name: "mcp/malformed JSON", method: http.MethodPost, target: mcpURL, body: `{"jsonrpc":`, client: served(200, podList), units: 5, tracker: budget.NewTracker()},
		{name: "mcp/tools/call served", method: http.MethodPost, target: mcpURL, body: listPods("demo"), client: served(200, podList), units: 5, tracker: budget.NewTracker()},
		{name: "mcp/tools/call invalid namespace", method: http.MethodPost, target: mcpURL, body: listPods("Not_Valid"), client: served(200, podList),
			units: 5, tracker: budget.NewTracker()},
		{name: "mcp/tools/call namespace outside policy", method: http.MethodPost, target: mcpURL, body: listPods("other"), client: served(200, podList),
			units: 5, tracker: budget.NewTracker()},
		{name: "mcp/tools/call budget exhausted", method: http.MethodPost, target: mcpURL, body: listPods("demo"), client: served(200, podList),
			units: 1, tracker: budget.NewTracker(), spent: true},
		{name: "mcp/tools/call upstream non-2xx", method: http.MethodPost, target: mcpURL, body: listPods("demo"), client: served(500, `{"kind":"Status"}`),
			units: 5, tracker: budget.NewTracker()},
	}
	lines := make([]string, 0, len(cases))
	for _, tc := range cases {
		events := &recordedEvents{}
		config := kubernetes.HandlerConfig{Upstream: upstream, Client: tc.client, MaxResponseBytes: 8 << 20, AuditLogger: events}
		if tc.noUpstream {
			config.Upstream, config.Client = nil, served(200, podList)
		}
		routes := gateway.Dispatcher{kubernetes.Authority: {Capability: capability, Tier: audit.TierSemantic,
			Handler: kubernetes.NewHandler(config), Budgets: tc.tracker, AuditLogger: events}}
		ledgerKey := ""
		if tc.tracker != nil {
			ledgerKey = key
		}
		if tc.spent {
			if err := tc.tracker.Meter(key, api.EndpointBudget{MaxUnits: 1}).SpendRequest(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		before := ledgerReading(tc.tracker, ledgerKey)
		var body io.Reader
		if tc.body != "" {
			body = strings.NewReader(tc.body)
		}
		req := httptest.NewRequest(tc.method, tc.target, body)
		if tc.body != "" {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
		}
		for k, v := range tc.header {
			req.Header.Set(k, v)
		}
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, req.WithContext(gateway.WithIdentity(req.Context(), grant(tc.units))))
		lines = append(lines, outcomeLine(t, tc.name, response, before+" -> "+ledgerReading(tc.tracker, ledgerKey), events.taken()))
	}
	compareRecorded(t, filepath.Join("testdata", "outcomes.txt"), lines)
}
