package gateway_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

// dispatchWith serves one request through a declared route whose handler
// runs serve, and returns what was recorded and what the decision kept.
func dispatchWith(t *testing.T, serve func(*http.Request)) (events []audit.Event, reason gateway.UnrecordedReason, declared, recorded bool) {
	t.Helper()
	writer := &recordedEvents{}
	d := gateway.Dispatcher{fixtureAuthority: {Capability: api.CapabilityKubernetesRead, AuditLogger: writer,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			serve(r)
			reason, declared, recorded = gateway.DeclaredUnrecorded(r.Context())
			w.WriteHeader(http.StatusNoContent)
		})}}
	grant := &gateway.ExecutionGrant{Execution: gateway.Reference{Backend: gateway.BackendDocker, UID: fixtureExecutionUID},
		Policy:       gateway.PolicySnapshot{Name: fixturePolicyName, Spec: api.AgentPolicySpec{AllowedCapabilities: []api.CapabilityKind{api.CapabilityKubernetesRead}}},
		Template:     gateway.TemplateSnapshot{Name: fixtureTemplateName},
		Capabilities: []api.CapabilityKind{api.CapabilityKubernetesRead}}
	r := httptest.NewRequest(http.MethodGet, serviceTarget, nil)
	d.ServeHTTP(httptest.NewRecorder(), r.WithContext(gateway.WithIdentity(r.Context(), grant)))
	return writer.taken(), reason, declared, recorded
}

// A handler built without an audit writer records nothing, so its request
// stays undecided and the dispatcher records the omission through the route.
func TestRecordWithoutAWriterLeavesTheRequestToTheFallback(t *testing.T) {
	events, _, _, recorded := dispatchWith(t, func(r *http.Request) {
		gateway.Record(r.Context(), nil, gateway.AuditEvent(r.Context(), "fixture.read"))
	})
	if recorded || len(events) != 1 || events[0].Outcome != audit.Unrecorded {
		t.Fatalf("a nil writer decided the request: recorded=%v events=%+v", recorded, events)
	}
}

// Only the listed reasons declare a request unrecorded; the decision keeps
// the reason, and a zero reason declares nothing.
func TestOnlyListedReasonsDeclareARequestUnrecorded(t *testing.T) {
	listed := gateway.UnrecordedReasons()
	if len(listed) != 3 || listed[0] == listed[1] || listed[1] == listed[2] || listed[0] == listed[2] ||
		listed[0] != gateway.UnrecordedMCPDiscovery() || listed[1] != gateway.UnrecordedKubernetesMCP() ||
		listed[2] != gateway.UnrecordedControl() {
		t.Fatalf("the reasons are not three distinct listed values: %v", listed)
	}
	for _, want := range gateway.UnrecordedReasons() {
		events, reason, declared, _ := dispatchWith(t, func(r *http.Request) { gateway.Unrecorded(r.Context(), want) })
		if !declared || reason != want || len(events) != 0 {
			t.Errorf("%s: declared=%v reason=%q events=%+v", want, declared, reason, events)
		}
	}
	events, _, declared, _ := dispatchWith(t, func(r *http.Request) { gateway.Unrecorded(r.Context(), gateway.UnrecordedReason{}) })
	if declared || len(events) != 1 || events[0].Outcome != audit.Unrecorded {
		t.Errorf("a zero reason declared the request: declared=%v events=%+v", declared, events)
	}
}
