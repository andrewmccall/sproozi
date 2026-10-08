package budget_test

import (
	"context"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
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

func TestReservationForfeitConsumesUnitsAndCostAcrossLedgers(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		ledger budget.Ledger
	}{
		{"memory", budget.NewMemoryLedger()},
		{"kubernetes", budget.NewKubernetesLedger(fake.NewClientBuilder().WithScheme(scheme).Build(), "ledger")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracker := budget.NewTrackerWithLedger(tc.ledger)
			limits := api.EndpointBudget{MaxUnits: 100, MaxCostMicros: 200}
			reservation, err := tracker.Reserve("missing-model-usage", limits, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := reservation.Forfeit(); err != nil {
				t.Fatal(err)
			}
			want := budget.State{UnitsUsed: 100, CostMicros: 200}
			if got := tracker.State("missing-model-usage"); got != want {
				t.Fatalf("forfeited accounting = %+v, want %+v", got, want)
			}
			for _, remainingLimit := range []api.EndpointBudget{{MaxUnits: 100}, {MaxCostMicros: 200}} {
				if _, err := tracker.Reserve("missing-model-usage", remainingLimit, 1, 1); err == nil {
					t.Fatal("forfeited capacity became available again")
				}
			}
			for _, repeat := range []func() error{reservation.Forfeit, reservation.Release, func() error { return reservation.Settle(0, 0) }} {
				if err := repeat(); err == nil {
					t.Fatal("settled reservation accepted a second transition")
				}
			}
			if got := tracker.State("missing-model-usage"); got != want {
				t.Fatalf("repeat changed accounting to %+v", got)
			}
		})
	}
}

func TestNilReservationForfeitRejectsMissingAdmission(t *testing.T) {
	var reservation *budget.Reservation
	if err := reservation.Forfeit(); err == nil {
		t.Fatal("nil reservation accepted forfeiture")
	}
}
