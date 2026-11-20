package gateway_test

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

// TestDispatcherAndLeakGuardOutcomesAreRecorded records what the dispatcher
// and LeakGuard reply, charge and audit for each way a request can end before
// or around its handler.
func TestDispatcherAndLeakGuardOutcomesAreRecorded(t *testing.T) {
	const capability = api.CapabilityKubernetesRead
	const accountKey = "managed-account"
	baseGrant := func(capabilities, allowed []api.CapabilityKind, units int64) *gateway.ExecutionGrant {
		g := &gateway.ExecutionGrant{Execution: gateway.Reference{Backend: gateway.BackendDocker, UID: fixtureExecutionUID, Name: "run"},
			Policy:       gateway.PolicySnapshot{Name: fixturePolicyName, Generation: 3, Spec: api.AgentPolicySpec{AllowedCapabilities: allowed}},
			Template:     gateway.TemplateSnapshot{Name: fixtureTemplateName},
			Capabilities: capabilities}
		if units > 0 {
			g.Policy.Spec.Budgets = map[api.CapabilityKind]api.EndpointBudget{capability: {MaxUnits: units}}
		}
		return g
	}
	granted := []api.CapabilityKind{capability}
	managed := func(allocation *gateway.Allocation) *gateway.ExecutionGrant {
		g := baseGrant(granted, granted, 2)
		g.Managed = true
		g.Allocations = map[api.CapabilityKind]gateway.Allocation{}
		if allocation != nil {
			g.Allocations[capability] = *allocation
		}
		return g
	}
	ceiling := api.EndpointBudget{MaxUnits: 2}

	// recording writes an allowed event through the route's writer and spends
	// one unit when the request carries a meter; silent records nothing.
	recording := func(events audit.Writer) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			meter := budget.FromContext(r.Context())
			spent := "none"
			if meter != nil {
				spent = "ok"
				if err := meter.SpendRequest(r.Context()); err != nil {
					spent = err.Error()
				}
			}
			e := gateway.AuditEvent(r.Context(), "fixture.read")
			e.Allowed = true
			events.Log(e)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "served meter="+strconv.FormatBool(meter != nil)+" spend="+spent)
		})
	}
	silent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "served without a record")
	})

	type dispatchCase struct {
		name    string
		routes  func(events *recordedEvents, tracker *budget.Tracker) gateway.Dispatcher
		grant   *gateway.ExecutionGrant
		target  string
		tracker *budget.Tracker
		ledger  string
		prepare func(*budget.Tracker)
	}
	capabilityRoute := func(handler func(*recordedEvents) http.Handler, logged bool) func(*recordedEvents, *budget.Tracker) gateway.Dispatcher {
		return func(events *recordedEvents, tracker *budget.Tracker) gateway.Dispatcher {
			route := gateway.Route{Capability: string(capability), Tier: audit.TierSemantic, Handler: handler(events), Budgets: tracker}
			if logged {
				route.AuditLogger = events
			}
			return gateway.Dispatcher{fixtureAuthority: route}
		}
	}
	recordingHandler := func(events *recordedEvents) http.Handler { return recording(events) }
	silentHandler := func(*recordedEvents) http.Handler { return silent }
	initialized := func(tracker *budget.Tracker) {
		if err := tracker.Initialize(accountKey); err != nil {
			t.Fatal(err)
		}
	}
	unmanagedKey := fixtureExecutionUID + "/" + string(capability)
	cases := []dispatchCase{
		{name: "dispatcher/no identity", routes: capabilityRoute(recordingHandler, true), target: "https://service.test/x"},
		{name: "dispatcher/unknown authority", routes: capabilityRoute(recordingHandler, true),
			grant: baseGrant(granted, granted, 0), target: "https://other.test/x"},
		{name: "dispatcher/handler-less host beside a path route", grant: baseGrant(granted, granted, 0),
			target: "https://service.test/repos/o/r",
			routes: func(events *recordedEvents, tracker *budget.Tracker) gateway.Dispatcher {
				return gateway.Dispatcher{fixtureAuthority: {Paths: []gateway.PathRoute{{Prefix: "/login/", Route: gateway.Route{
					Capability: string(capability), Tier: audit.TierSemantic, Handler: recording(events), AuditLogger: events}}}}}
			}},
		{name: "dispatcher/path route served", grant: baseGrant(granted, granted, 0), target: "https://service.test/login/device",
			routes: func(events *recordedEvents, tracker *budget.Tracker) gateway.Dispatcher {
				return gateway.Dispatcher{fixtureAuthority: {Paths: []gateway.PathRoute{{Prefix: "/login/", Route: gateway.Route{
					Capability: string(capability), Tier: audit.TierSemantic, Handler: recording(events), AuditLogger: events}}}}}
			}},
		{name: "dispatcher/capability not requested by the run", routes: capabilityRoute(recordingHandler, true),
			grant: baseGrant(nil, granted, 0), target: "https://service.test/x"},
		{name: "dispatcher/capability not allowed by policy", routes: capabilityRoute(recordingHandler, true),
			grant: baseGrant(granted, nil, 0), target: "https://service.test/x"},
		{name: "dispatcher/capability denied on a route without a writer", routes: capabilityRoute(recordingHandler, false),
			grant: baseGrant(nil, granted, 0), target: "https://service.test/x"},
		{name: "dispatcher/budget configured without a tracker", routes: capabilityRoute(recordingHandler, true),
			grant: baseGrant(granted, granted, 1), target: "https://service.test/x"},
		{name: "dispatcher/managed account missing", routes: capabilityRoute(recordingHandler, true),
			grant:   managed(&gateway.Allocation{AccountKey: accountKey, Ceiling: ceiling}),
			target:  "https://service.test/x",
			tracker: budget.NewTracker(), ledger: accountKey},
		{name: "dispatcher/managed allocation exhausted", routes: capabilityRoute(recordingHandler, true),
			grant:   managed(&gateway.Allocation{AccountKey: accountKey, Ceiling: ceiling, Exhausted: true}),
			target:  "https://service.test/x",
			tracker: budget.NewTracker(), ledger: accountKey, prepare: initialized},
		{name: "dispatcher/managed allocation absent from the grant", routes: capabilityRoute(recordingHandler, true),
			grant: managed(nil), target: "https://service.test/x", tracker: budget.NewTracker()},
		{name: "dispatcher/managed ledger unavailable", routes: capabilityRoute(recordingHandler, true),
			grant:   managed(&gateway.Allocation{AccountKey: accountKey, Ceiling: ceiling}),
			target:  "https://service.test/x",
			tracker: budget.NewTrackerWithLedger(failingLedger{})},
		{name: "dispatcher/managed allocation served", routes: capabilityRoute(recordingHandler, true),
			grant:   managed(&gateway.Allocation{AccountKey: accountKey, Ceiling: ceiling}),
			target:  "https://service.test/x",
			tracker: budget.NewTracker(), ledger: accountKey, prepare: initialized},
		{name: "dispatcher/unmanaged budget served", routes: capabilityRoute(recordingHandler, true),
			grant: baseGrant(granted, granted, 1), target: "https://service.test/x",
			tracker: budget.NewTracker(), ledger: unmanagedKey},
		{name: "dispatcher/unmanaged budget already spent", routes: capabilityRoute(recordingHandler, true),
			grant: baseGrant(granted, granted, 1), target: "https://service.test/x",
			tracker: budget.NewTracker(), ledger: unmanagedKey, prepare: func(tracker *budget.Tracker) {
				if err := tracker.Meter(unmanagedKey, api.EndpointBudget{MaxUnits: 1}).SpendRequest(context.Background()); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "dispatcher/no budget configured", routes: capabilityRoute(recordingHandler, true),
			grant: baseGrant(granted, granted, 0), target: "https://service.test/x", tracker: budget.NewTracker()},
		{name: "dispatcher/handler records nothing", routes: capabilityRoute(silentHandler, true),
			grant: baseGrant(granted, granted, 0), target: "https://service.test/x"},
		{name: "dispatcher/route without a capability, nothing granted", grant: baseGrant(nil, nil, 0),
			target: "https://service.test/x", tracker: budget.NewTracker(),
			routes: func(events *recordedEvents, tracker *budget.Tracker) gateway.Dispatcher {
				return gateway.Dispatcher{fixtureAuthority: {Tier: audit.TierProtocol, Handler: recording(events),
					Budgets: tracker, AuditLogger: events}}
			}},
	}
	var lines []string
	for _, tc := range cases {
		events := &recordedEvents{}
		if tc.prepare != nil {
			tc.prepare(tc.tracker)
		}
		before := ledgerReading(tc.tracker, tc.ledger)
		req := httptest.NewRequest(http.MethodGet, tc.target, nil)
		if tc.grant != nil {
			req = req.WithContext(gateway.WithIdentity(req.Context(), tc.grant))
		}
		response := httptest.NewRecorder()
		tc.routes(events, tc.tracker).ServeHTTP(response, req)
		ledger := before + " -> " + ledgerReading(tc.tracker, tc.ledger)
		lines = append(lines, outcomeLine(t, tc.name, response, ledger, events.taken()))
	}

	// LeakGuard: the provider route remembers the workload's credential, and
	// every other route refuses it.
	const credential = "sk-ant-oat01-recorded-workload-credential"
	leakCases := baseGrant([]api.CapabilityKind{api.CapabilityModelInference, api.CapabilityNetworkEgress},
		[]api.CapabilityKind{api.CapabilityModelInference, api.CapabilityNetworkEgress}, 0)
	for _, tc := range []struct {
		name    string
		target  string
		body    string
		prepare func(*http.Request)
	}{
		{name: "leakguard/provider route remembers the credential", target: "https://provider.test/v1/messages",
			prepare: func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+credential) }},
		{name: "leakguard/other route without the credential", target: "https://other.test/upload", body: "ordinary"},
		{name: "leakguard/credential in a header", target: "https://other.test/upload",
			prepare: func(r *http.Request) { r.Header.Set("X-Note", "token="+credential) }},
		{name: "leakguard/credential in the query", target: "https://other.test/upload?q=" + credential},
		{name: "leakguard/credential in the body", target: "https://other.test/upload", body: "before " + credential + " after"},
		{name: "leakguard/provider route again with the credential", target: "https://provider.test/v1/messages",
			prepare: func(r *http.Request) { r.Header.Set("X-Api-Key", credential) }},
	} {
		events := &recordedEvents{}
		reader := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, err := io.Copy(io.Discard, r.Body)
			e := gateway.AuditEvent(r.Context(), "fixture.upload")
			e.Allowed = err == nil
			if err != nil {
				e.DenyReason = err.Error()
			}
			events.Log(e)
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, "read error=%v", err)
		})
		guard := leakGuardFixture(t, events, reader)
		req := httptest.NewRequest(http.MethodPost, tc.target, strings.NewReader(tc.body))
		if tc.prepare != nil {
			tc.prepare(req)
		}
		req = req.WithContext(gateway.WithIdentity(req.Context(), leakCases))
		response := httptest.NewRecorder()
		guard.ServeHTTP(response, req)
		lines = append(lines, outcomeLine(t, tc.name, response, "-", events.taken()))
	}
	compareRecorded(t, filepath.Join("testdata", "outcomes.txt"), lines)
}

// leakGuardFixtures keeps one LeakGuard across the LeakGuard cases, so the
// credential the provider route observed is refused on the others.
var leakGuardFixtures = map[*testing.T]*leakFixture{}

type leakFixture struct {
	guard  *gateway.LeakGuard
	events *recordedEvents
	other  http.Handler
}

func leakGuardFixture(t *testing.T, events *recordedEvents, other http.Handler) *gateway.LeakGuard {
	f := leakGuardFixtures[t]
	if f == nil {
		f = &leakFixture{}
		ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			e := gateway.AuditEvent(r.Context(), "fixture.model")
			e.Allowed = true
			f.events.Log(e)
			w.WriteHeader(http.StatusNoContent)
		})
		forward := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.other.ServeHTTP(w, r) })
		f.guard = gateway.NewLeakGuard(gateway.Dispatcher{
			providerAuthority: {Capability: string(api.CapabilityModelInference), Tier: audit.TierSemantic,
				Handler: ok, ProviderCredential: true, AuditLogger: logTo(f)},
			otherAuthority: {Capability: string(api.CapabilityNetworkEgress), Tier: audit.TierDestination,
				Handler: forward, AuditLogger: logTo(f)},
		}, nil)
		leakGuardFixtures[t] = f
		t.Cleanup(func() { delete(leakGuardFixtures, t) })
	}
	f.events, f.other = events, other
	return f.guard
}

// logTo writes to whichever case's events the fixture holds now.
type fixtureWriter struct{ f *leakFixture }

func (w fixtureWriter) Log(e audit.Event) { w.f.events.Log(e) }

func logTo(f *leakFixture) audit.Writer { return fixtureWriter{f} }
