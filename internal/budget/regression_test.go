package budget_test

import (
	"context"
	"testing"

	"github.com/andrewmccall/sproozi/internal/budget"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestLedgerAdaptersAgreeOnFailedSettlement(t *testing.T) {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	mem := budget.NewMemoryLedger()
	kube := budget.NewKubernetesLedger(fake.NewClientBuilder().WithScheme(s).Build(), "ledger")
	ctx := context.Background()
	for _, l := range []budget.Ledger{mem, kube} {
		r, err := l.Reserve(ctx, "review", budget.UsageReservation{MaxUnits: 100, Units: 10})
		if err != nil {
			t.Fatal(err)
		}
		if err = l.Settle(ctx, r, budget.TrustedUsage{Units: 11}); err == nil {
			t.Fatal("expected over-settlement error")
		}
	}
	a, b := mem.State(ctx, "review"), kube.State(ctx, "review")
	if a != b {
		t.Fatalf("same contract, different failed-settlement state: memory=%+v kubernetes=%+v", a, b)
	}
}
