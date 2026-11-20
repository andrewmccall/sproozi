package destination_test

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

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/endpoints/destination"
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

// TestDestinationOutcomesAreRecorded records what a network.egress route
// replies, charges and audits for each way a request ends.
func TestDestinationOutcomesAreRecorded(t *testing.T) {
	const capability = api.CapabilityNetworkEgress
	const authority = "registry.example:8443"
	profiles, err := destination.LoadProfileStore([]byte(`{"registry":[{"scheme":"https","host":"registry.example","port":8443}]}`))
	if err != nil {
		t.Fatal(err)
	}
	grant := func(units int64) *gateway.ExecutionGrant {
		g := &gateway.ExecutionGrant{Execution: gateway.Reference{Backend: gateway.BackendDocker, Name: "run", UID: "test-execution"},
			Policy: gateway.PolicySnapshot{Name: "test-policy", Generation: 4, Spec: api.AgentPolicySpec{
				AllowedCapabilities: []api.CapabilityKind{capability}, EgressProfiles: []string{"registry"}}},
			Template:     gateway.TemplateSnapshot{Name: "test-template", Spec: api.AgentTemplateSpec{EgressProfiles: []string{"registry"}}},
			Capabilities: []api.CapabilityKind{capability}}
		if units > 0 {
			g.Policy.Spec.Budgets = map[api.CapabilityKind]api.EndpointBudget{capability: {MaxUnits: units}}
		}
		return g
	}
	served := func(status int, body string) *http.Client {
		return &http.Client{Transport: recordTransport(func(r *http.Request) (*http.Response, error) {
			return recordResponse(r, status, http.Header{"Content-Type": {"text/plain"}}, body), nil
		})}
	}
	failing := &http.Client{Transport: recordTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed")
	})}
	key := "test-execution/" + string(capability)
	cases := []struct {
		name, method, target, host string
		client                     *http.Client
		units                      int64
		tracker                    *budget.Tracker
		spent                      bool
	}{
		{name: "plain HTTP is not inspected HTTPS", method: http.MethodGet, target: "http://registry.example:8443/pkg", client: served(200, "x")},
		{name: "CONNECT is not inspected HTTPS", method: http.MethodConnect, target: "https://registry.example:8443/pkg", host: authority, client: served(200, "x")},
		{name: "IP literal authority", method: http.MethodGet, target: "https://127.0.0.1:8443/pkg", host: authority, client: served(200, "x")},
		{name: "destination not approved", method: http.MethodGet, target: "https://evil.example:8443/pkg", host: authority, client: served(200, "x")},
		{name: "budget exhausted", method: http.MethodGet, target: "https://registry.example:8443/pkg", client: served(200, "x"),
			units: 1, tracker: budget.NewTracker(), spent: true},
		{name: "ledger unavailable", method: http.MethodGet, target: "https://registry.example:8443/pkg", client: served(200, "x"),
			units: 1, tracker: budget.NewTrackerWithLedger(failingLedger{})},
		{name: "upstream transport error", method: http.MethodGet, target: "https://registry.example:8443/pkg", client: failing,
			units: 5, tracker: budget.NewTracker()},
		{name: "upstream non-2xx", method: http.MethodGet, target: "https://registry.example:8443/pkg", client: served(404, "missing"),
			units: 5, tracker: budget.NewTracker()},
		{name: "served", method: http.MethodGet, target: "https://registry.example:8443/pkg", client: served(200, "package"),
			units: 5, tracker: budget.NewTracker()},
	}
	var lines []string
	for _, tc := range cases {
		events := &recordedEvents{}
		handler := &destination.Handler{Config: destination.HandlerConfig{Profiles: profiles, AuditLogger: events, HTTPClient: tc.client}}
		route := gateway.Route{Capability: string(capability), Tier: audit.TierDestination, Handler: handler,
			Budgets: tc.tracker, AuditLogger: events}
		routes := gateway.Dispatcher{authority: route, "evil.example:8443": route, "127.0.0.1:8443": route}
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
		req := httptest.NewRequest(tc.method, tc.target, nil)
		if tc.host != "" {
			req.Host = tc.host
		}
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, req.WithContext(gateway.WithIdentity(req.Context(), grant(tc.units))))
		lines = append(lines, outcomeLine(t, tc.name, response, before+" -> "+ledgerReading(tc.tracker, ledgerKey), events.taken()))
	}
	compareRecorded(t, filepath.Join("testdata", "outcomes.txt"), lines)
}
