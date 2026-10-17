package gateway_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

func dockerGrant() *gateway.ExecutionGrant {
	return &gateway.ExecutionGrant{
		Execution: gateway.Reference{Backend: gateway.BackendDocker, Name: "local-visit", UID: "container-incarnation"},
		Policy: gateway.PolicySnapshot{Name: "local-policy", Spec: api.AgentPolicySpec{
			AllowedCapabilities: []api.CapabilityKind{api.CapabilityKubernetesRead},
			Budgets:             map[api.CapabilityKind]api.EndpointBudget{api.CapabilityKubernetesRead: {MaxUnits: 10}},
		}},
		Template:     gateway.TemplateSnapshot{Name: "codex-mobile"},
		Capabilities: []api.CapabilityKind{api.CapabilityKubernetesRead},
		Managed:      true,
		Allocations: map[api.CapabilityKind]gateway.Allocation{
			api.CapabilityKubernetesRead: {AccountKey: "issued-visit/kubernetes.read", Ceiling: api.EndpointBudget{MaxUnits: 2}},
		},
	}
}

func TestDockerGrantEnforcesInitializedAllocationWithoutAgentRun(t *testing.T) {
	grant := dockerGrant()
	if err := grant.Validate(); err != nil {
		t.Fatal(err)
	}
	tracker := budget.NewTracker()
	if err := tracker.Initialize("issued-visit/kubernetes.read"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	dispatch := gateway.Dispatcher{fixtureAuthority: {Capability: string(api.CapabilityKubernetesRead), Budgets: tracker, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := budget.FromContext(r.Context()).SpendRequest(r.Context()); err != nil {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		calls++
		w.WriteHeader(http.StatusNoContent)
	})}}
	request := func() int {
		r := httptest.NewRequest(http.MethodGet, "https://service.test/path", nil)
		w := httptest.NewRecorder()
		dispatch.ServeHTTP(w, r.WithContext(gateway.WithIdentity(r.Context(), grant)))
		return w.Code
	}
	for _, want := range []int{http.StatusNoContent, http.StatusNoContent, http.StatusTooManyRequests} {
		if got := request(); got != want {
			t.Fatalf("status=%d want=%d", got, want)
		}
	}
	if calls != 2 {
		t.Fatalf("forwarded %d requests", calls)
	}
	state, err := tracker.State("issued-visit/kubernetes.read")
	if err != nil || state.UnitsUsed != 2 {
		t.Fatalf("allocation accounting: %+v %v", state, err)
	}
	if event := grant.AuditEvent("read"); event.ExecutionBackend != "docker" || event.RunUID != "container-incarnation" || event.SAUID != "" {
		t.Fatalf("Docker audit invented Kubernetes authority: %+v", event)
	}
}

func TestManagedDispatchDeniesMissingOrExhaustedAllocations(t *testing.T) {
	for _, name := range []string{"missing account", "missing allocation", "exhausted", "zero units"} {
		t.Run(name, func(t *testing.T) {
			grant := dockerGrant()
			cap := api.CapabilityKubernetesRead
			allocation := grant.Allocations[cap]
			switch name {
			case "missing allocation":
				delete(grant.Allocations, cap)
			case "exhausted":
				allocation.Exhausted = true
				grant.Allocations[cap] = allocation
			case "zero units":
				allocation.Ceiling.MaxUnits = 0
				grant.Allocations[cap] = allocation
			}
			calls := 0
			dispatch := gateway.Dispatcher{fixtureAuthority: {Capability: string(cap), Budgets: budget.NewTracker(), Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ })}}
			r := httptest.NewRequest(http.MethodGet, "https://service.test/path", nil)
			w := httptest.NewRecorder()
			dispatch.ServeHTTP(w, r.WithContext(gateway.WithIdentity(r.Context(), grant)))
			if w.Code != http.StatusForbidden || calls != 0 {
				t.Fatalf("allocation denial: status=%d forwarded=%d", w.Code, calls)
			}
		})
	}
}

func TestBudgetSelectionKeepsPolicyAndAllocatedCeilings(t *testing.T) {
	grant := dockerGrant()
	cap := api.CapabilityKubernetesRead
	grant.Allocations[cap] = gateway.Allocation{AccountKey: "visit/read", Ceiling: api.EndpointBudget{MaxUnits: 100, MaxCostMicros: 200}}
	for _, tc := range []struct{ policy, want api.EndpointBudget }{
		{api.EndpointBudget{}, api.EndpointBudget{MaxUnits: 100, MaxCostMicros: 200}},
		{api.EndpointBudget{MaxUnits: 5, MaxCostMicros: 20}, api.EndpointBudget{MaxUnits: 5, MaxCostMicros: 20}},
		{api.EndpointBudget{MaxUnits: 200, MaxCostMicros: 300}, api.EndpointBudget{MaxUnits: 100, MaxCostMicros: 200}},
	} {
		grant.Policy.Spec.Budgets[cap] = tc.policy
		limits, configured, err := grant.BudgetLimits(cap)
		if err != nil || !configured || limits != tc.want {
			t.Fatalf("policy=%+v result=%+v configured=%v err=%v", tc.policy, limits, configured, err)
		}
	}
	grant.Allocations[cap] = gateway.Allocation{AccountKey: "visit/read", Ceiling: api.EndpointBudget{MaxUnits: 100}}
	grant.Policy.Spec.Budgets[cap] = api.EndpointBudget{MaxCostMicros: 20}
	limits, _, err := grant.BudgetLimits(cap)
	if err != nil || limits.MaxCostMicros != 20 {
		t.Fatalf("live monetary limit removed: %+v %v", limits, err)
	}
}

func TestGrantCloneDoesNotShareAdministratorSnapshots(t *testing.T) {
	grant := dockerGrant()
	copy := grant.Clone()
	grant.Policy.Spec.Budgets[api.CapabilityKubernetesRead] = api.EndpointBudget{MaxUnits: 1000}
	grant.Capabilities[0] = api.CapabilityModelInference
	delete(grant.Allocations, api.CapabilityKubernetesRead)
	if copy.Policy.Spec.Budgets[api.CapabilityKubernetesRead].MaxUnits != 10 || copy.Capabilities[0] != api.CapabilityKubernetesRead || len(copy.Allocations) != 1 {
		t.Fatal("transport snapshot retained mutable input aliases")
	}
}
