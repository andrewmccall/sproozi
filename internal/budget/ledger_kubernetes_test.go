package budget_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/andrewmccall/sproozi/internal/budget"
)

func TestKubernetesLedgerSurvivesNewInstance(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	l1 := budget.NewKubernetesLedger(c, "ledger")
	r, err := l1.Reserve(context.Background(), "run-restart", budget.UsageReservation{MaxUnits: 100, Units: 60})
	if err != nil {
		t.Fatal(err)
	}
	if err := l1.Settle(context.Background(), r, budget.TrustedUsage{Units: 60}); err != nil {
		t.Fatal(err)
	}
	l2 := budget.NewKubernetesLedger(c, "ledger")
	if got := l2.State(context.Background(), "run-restart").UnitsUsed; got != 60 {
		t.Fatalf("restarted ledger usage = %d, want 60", got)
	}
}

func TestKubernetesLedgerCompetingReplicasCannotOversubscribe(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	a := budget.NewKubernetesLedger(c, "ledger")
	b := budget.NewKubernetesLedger(c, "ledger")
	if _, err := a.Reserve(context.Background(), "run-replicas", budget.UsageReservation{MaxUnits: 100, Units: 60}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reserve(context.Background(), "run-replicas", budget.UsageReservation{MaxUnits: 100, Units: 60}); err == nil {
		t.Fatal("second replica oversubscribed the hard ceiling")
	}
}
