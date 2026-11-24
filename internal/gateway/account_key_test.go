package gateway_test

import (
	"context"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
	budgetkubernetes "github.com/andrewmccall/sproozi/internal/budget/kubernetes"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A grant charges usage under the key budget.Account builds, and the
// Kubernetes ledger reads the run UID and capability back from that key into
// the BudgetLedger it creates.
func TestGrantAccountKeyIsReadBackByTheKubernetesLedger(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	tracker := budget.NewTrackerWithLedger(budgetkubernetes.NewLedger(c, "ledgers"))
	const uid, capability = "0c1f9b1e-6a52-4a3c-9a43-1d2e3f405162", api.CapabilityModelInference
	grant := &gateway.ExecutionGrant{
		Execution: gateway.Reference{UID: uid},
		Policy: gateway.PolicySnapshot{Spec: api.AgentPolicySpec{
			Budgets: map[api.CapabilityKind]api.EndpointBudget{capability: {MaxUnits: 10}}}},
	}
	meter, err := grant.Meter(context.Background(), tracker, capability)
	if err != nil || meter == nil {
		t.Fatalf("Meter = %v, %v", meter, err)
	}
	if err = meter.Spend(context.Background(), 3, 0); err != nil {
		t.Fatal(err)
	}
	var ledgers api.BudgetLedgerList
	if err = c.List(context.Background(), &ledgers); err != nil || len(ledgers.Items) != 1 {
		t.Fatalf("ledgers = %d, %v; want one", len(ledgers.Items), err)
	}
	spec := ledgers.Items[0].Spec
	if spec.Account != budget.Account(uid, string(capability)) || spec.RunUID != uid ||
		spec.Capability != string(capability) || spec.UnitsUsed != 3 {
		t.Fatalf("ledger spec = %+v, want account %q for run %q and capability %q with 3 units",
			spec, budget.Account(uid, string(capability)), uid, capability)
	}
}
