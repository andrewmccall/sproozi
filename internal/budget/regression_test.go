package budget_test

import (
	"context"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
	budgetkubernetes "github.com/andrewmccall/sproozi/internal/budget/kubernetes"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestLedgerAdaptersAgreeOnFailedSettlement(t *testing.T) {
	s := runtime.NewScheme()
	if err := api.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	mem := budget.NewMemoryLedger()
	kube := budgetkubernetes.NewLedger(fake.NewClientBuilder().WithScheme(s).Build(), "ledger")
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
	a, b := observedLedgerState(t, mem, ctx, "review"), observedLedgerState(t, kube, ctx, "review")
	if a != b {
		t.Fatalf("same contract, different failed-settlement state: memory=%+v kubernetes=%+v", a, b)
	}
}

func TestReservationForfeitConsumesUnitsAndCostAcrossLedgers(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		ledger budget.Ledger
	}{
		{"memory", budget.NewMemoryLedger()},
		{"kubernetes", budgetkubernetes.NewLedger(fake.NewClientBuilder().WithScheme(scheme).Build(), "ledger")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracker := budget.NewTrackerWithLedger(tc.ledger)
			limits := api.EndpointBudget{MaxUnits: 100, MaxCostMicros: 200}
			reservation, err := tracker.Meter("missing-model-usage", limits).Reserve(t.Context(), 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := reservation.Forfeit(); err != nil {
				t.Fatal(err)
			}
			want := budget.State{UnitsUsed: 100, CostMicros: 200}
			if got := observedState(t, tracker, "missing-model-usage"); got != want {
				t.Fatalf("forfeited accounting = %+v, want %+v", got, want)
			}
			for _, remainingLimit := range []api.EndpointBudget{{MaxUnits: 100}, {MaxCostMicros: 200}} {
				if _, err := tracker.Meter("missing-model-usage", remainingLimit).Reserve(t.Context(), 1, 1); err == nil {
					t.Fatal("forfeited capacity became available again")
				}
			}
			for _, repeat := range []func() error{reservation.Forfeit, reservation.Release, func() error { return reservation.Settle(0, 0) }} {
				if err := repeat(); err == nil {
					t.Fatal("settled reservation accepted a second transition")
				}
			}
			if got := observedState(t, tracker, "missing-model-usage"); got != want {
				t.Fatalf("repeat changed accounting to %+v", got)
			}
		})
	}
}

// A forfeit charges only the dimensions with a limit: with no unit limit, the
// request's output bound is not recorded as units used.
func TestReservationForfeitChargesOnlyLimitedDimensions(t *testing.T) {
	tracker := budget.NewTracker()
	reservation, err := tracker.Meter("cost-only", api.EndpointBudget{MaxCostMicros: 200}).Reserve(t.Context(), 4096, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = reservation.Forfeit(); err != nil {
		t.Fatal(err)
	}
	if got, want := observedState(t, tracker, "cost-only"), (budget.State{CostMicros: 200}); got != want {
		t.Fatalf("forfeited accounting = %+v, want %+v", got, want)
	}
	reservation, err = tracker.Meter("units-only", api.EndpointBudget{MaxUnits: 100}).Reserve(t.Context(), 10, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err = reservation.Forfeit(); err != nil {
		t.Fatal(err)
	}
	if got, want := observedState(t, tracker, "units-only"), (budget.State{UnitsUsed: 10}); got != want {
		t.Fatalf("forfeited accounting = %+v, want %+v", got, want)
	}
}

func TestNilReservationForfeitRejectsMissingAdmission(t *testing.T) {
	var reservation *budget.Reservation
	if err := reservation.Forfeit(); err == nil {
		t.Fatal("nil reservation accepted forfeiture")
	}
}
