package packages_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/andrewmccall/sproozi/internal/endpoints/packages"
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

// TestPackageOutcomesAreRecorded records what a packages.install route
// replies, charges and audits for each way a request ends.
func TestPackageOutcomesAreRecorded(t *testing.T) {
	const capability = api.CapabilityPackagesInstall
	const wheelName = "demo_tool-1.0.0-py3-none-any.whl"
	const wheelURL = "https://files.pythonhosted.org/packages/f/" + wheelName
	const indexURL = "https://pypi.org/simple/demo-tool/"
	wheel := []byte("fixture-wheel")
	digest := sha256.Sum256(wheel)
	wheelDigest := hex.EncodeToString(digest[:])
	metadata := `<a href="` + wheelURL + `#sha256=` + wheelDigest + `">` + wheelName + `</a>`
	grant := func(units, cost int64) *gateway.ExecutionGrant {
		g := &gateway.ExecutionGrant{Execution: gateway.Reference{Backend: gateway.BackendKubernetes, Namespace: "agents", Name: "run", UID: "test-execution"},
			Authority: gateway.Reference{Backend: gateway.BackendKubernetes, Namespace: "agents", Name: "runner", UID: "sa-uid"},
			Policy: gateway.PolicySnapshot{Name: "test-policy", Generation: 2, Spec: api.AgentPolicySpec{
				AllowedCapabilities: []api.CapabilityKind{capability},
				PackagesInstall: api.PackagesInstallScope{Ecosystems: []api.PackageEcosystem{api.PackageEcosystemPyPI}, MaxDownloadBytes: 1024,
					Artifacts: []api.PackageArtifact{{Name: "demo-tool", Version: "1.0.0", Filename: wheelName, SHA256: wheelDigest}}},
			}},
			Template:     gateway.TemplateSnapshot{Name: "test-template"},
			Capabilities: []api.CapabilityKind{capability}}
		if units > 0 || cost > 0 {
			g.Policy.Spec.Budgets = map[api.CapabilityKind]api.EndpointBudget{capability: {MaxUnits: units, MaxCostMicros: cost}}
		}
		return g
	}
	served := func(status int, header http.Header, body string) *http.Client {
		return &http.Client{Transport: recordTransport(func(r *http.Request) (*http.Response, error) {
			return recordResponse(r, status, header, body), nil
		})}
	}
	failing := &http.Client{Transport: recordTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed")
	})}
	key := "test-execution/" + string(capability)
	cases := []struct {
		name, target string
		client       *http.Client
		grant        *gateway.ExecutionGrant
		tracker      *budget.Tracker
		spent        bool
	}{
		{name: "policy refuses an unlisted package", target: "https://pypi.org/simple/other/", client: served(200, nil, metadata), grant: grant(0, 0)},
		{name: "no client", target: indexURL, grant: grant(0, 0)},
		{name: "query is not canonical", target: indexURL + "?x=1", client: served(200, nil, metadata), grant: grant(0, 0)},
		{name: "budget exhausted", target: indexURL, client: served(200, nil, metadata), grant: grant(1, 0), tracker: budget.NewTracker(), spent: true},
		{name: "ledger unavailable", target: indexURL, client: served(200, nil, metadata), grant: grant(1, 0),
			tracker: budget.NewTrackerWithLedger(failingLedger{})},
		{name: "monetary cap on an unpriced endpoint", target: indexURL, client: served(200, nil, metadata), grant: grant(5, 100),
			tracker: budget.NewTracker()},
		{name: "upstream transport error", target: indexURL, client: failing, grant: grant(5, 0), tracker: budget.NewTracker()},
		{name: "upstream redirect", target: wheelURL, client: served(302, http.Header{"Location": {"https://evil.example/x.whl"}}, "moved"),
			grant: grant(5, 0), tracker: budget.NewTracker()},
		{name: "artifact digest mismatch", target: wheelURL, client: served(200, nil, "not-the-wheel"), grant: grant(5, 0), tracker: budget.NewTracker()},
		{name: "metadata with an unlisted artifact", target: indexURL,
			client: served(200, nil, `<a href="https://files.pythonhosted.org/packages/f/other-1.0-py3-none-any.whl">x</a>`),
			grant:  grant(5, 0), tracker: budget.NewTracker()},
		{name: "upstream non-2xx", target: indexURL, client: served(404, http.Header{"Content-Type": {"text/plain"}}, "not found"),
			grant: grant(5, 0), tracker: budget.NewTracker()},
		{name: "metadata served", target: indexURL, client: served(200, http.Header{"Content-Type": {"text/html"}}, metadata),
			grant: grant(5, 0), tracker: budget.NewTracker()},
		{name: "artifact served", target: wheelURL, client: served(200, http.Header{"Content-Type": {"application/octet-stream"}}, string(wheel)),
			grant: grant(5, 0), tracker: budget.NewTracker()},
	}
	var lines []string
	for _, tc := range cases {
		events := &recordedEvents{}
		handler := &packages.Handler{Config: packages.HandlerConfig{HTTPClient: tc.client, AuditLogger: events}}
		route := gateway.Route{Capability: capability, Tier: audit.TierSemantic, Handler: handler,
			Budgets: tc.tracker, AuditLogger: events}
		routes := gateway.Dispatcher{packages.IndexAuthority: route, packages.FilesAuthority: route}
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
		req := httptest.NewRequest(http.MethodGet, tc.target, nil)
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, req.WithContext(gateway.WithIdentity(req.Context(), tc.grant)))
		lines = append(lines, outcomeLine(t, tc.name, response, before+" -> "+ledgerReading(tc.tracker, ledgerKey), events.taken()))
	}
	compareRecorded(t, filepath.Join("testdata", "outcomes.txt"), lines)
}
