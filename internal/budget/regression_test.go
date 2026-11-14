package budget_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
	budgetfile "github.com/andrewmccall/sproozi/internal/budget/file"
	budgetkubernetes "github.com/andrewmccall/sproozi/internal/budget/kubernetes"
	"github.com/andrewmccall/sproozi/internal/testutil"
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

// After a seal, the receipt already holds every pending reservation in full,
// so a settlement above its reservation is charged the reservation and
// reported as exceeded. It must not fail and leave the reservation held.
func TestSettlementAfterSealIsChargedAtMostItsReservation(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	fileLedger, err := budgetfile.NewLedger(filepath.Join(testutil.PrivateDir(t), "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fileLedger.Close() })
	for _, tc := range []struct {
		name   string
		ledger budget.Ledger
	}{
		{"memory", budget.NewMemoryLedger()},
		{"file", fileLedger},
		{"kubernetes", budgetkubernetes.NewLedger(fake.NewClientBuilder().WithScheme(scheme).Build(), "ledger")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			const key budget.AccountKey = "visit/model.inference"
			if err := tc.ledger.Initialize(ctx, key); err != nil {
				t.Fatal(err)
			}
			r, err := tc.ledger.Reserve(ctx, key, budget.UsageReservation{MaxUnits: 100, MaxCostMicros: 1000,
				Units: 10, CostMicros: 50, RequireExisting: true,
				Recovery: &budget.Recovery{ExpiresAt: time.Now().Add(time.Hour), Units: 10, CostMicros: 50, Model: true}})
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := tc.ledger.Seal(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.ledger.Settle(ctx, r, budget.TrustedUsage{Units: 20, CostMicros: 80}); !errors.Is(err, budget.ErrUsageExceeded) {
				t.Fatalf("settlement above the reservation after a seal: err = %v, want ErrUsageExceeded", err)
			}
			if got, want := observedLedgerState(t, tc.ledger, ctx, key), (budget.State{UnitsUsed: 10, CostMicros: 50}); got != want {
				t.Fatalf("state after sealed settlement = %+v, want %+v", got, want)
			}
			if again, err := tc.ledger.Seal(ctx, key); err != nil || again != receipt {
				t.Fatalf("receipt after settlement = %+v, %v; want %+v", again, err, receipt)
			}
			if err := tc.ledger.Settle(ctx, r, budget.TrustedUsage{}); !errors.Is(err, budget.ErrAlreadySettled) {
				t.Fatalf("second settlement: err = %v, want ErrAlreadySettled", err)
			}
		})
	}
}

// After a seal, each dimension within its reservation is charged as reported
// and only the dimension above it is capped.
func TestSettlementAfterSealCapsEachDimensionAtItsReservation(t *testing.T) {
	ctx := t.Context()
	ledger := budget.NewMemoryLedger()
	const key budget.AccountKey = "visit/model.inference"
	r, err := ledger.Reserve(ctx, key, budget.UsageReservation{MaxUnits: 100, MaxCostMicros: 1000, Units: 10, CostMicros: 50})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Seal(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Settle(ctx, r, budget.TrustedUsage{Units: 4, CostMicros: 60}); !errors.Is(err, budget.ErrUsageExceeded) {
		t.Fatalf("cost above the reservation: err = %v, want ErrUsageExceeded", err)
	}
	if got, want := observedLedgerState(t, ledger, ctx, key), (budget.State{UnitsUsed: 4, CostMicros: 50}); got != want {
		t.Fatalf("state = %+v, want %+v", got, want)
	}
}
