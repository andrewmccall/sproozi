// internal/endpoints/model/budget_test.go
package budget_test

import (
	"context"
	"sync"
	"testing"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
)

func observedState(t *testing.T, tracker *budget.Tracker, key string) budget.State {
	t.Helper()
	state, err := tracker.State(key)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func observedLedgerState(t *testing.T, ledger budget.Ledger, ctx context.Context, key budget.AccountKey) budget.State {
	t.Helper()
	state, err := ledger.State(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func observedMeterState(t *testing.T, meter *budget.Meter) budget.State {
	t.Helper()
	state, err := meter.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func testBudget(maxUnits int64) sprooziv1alpha1.EndpointBudget {
	return sprooziv1alpha1.EndpointBudget{MaxUnits: maxUnits}
}

func TestBudgetReserveCountsInFlightCapacity(t *testing.T) {
	bt := budget.NewTracker()
	limits := testBudget(100)

	first, err := bt.Meter("run-reserve", limits).Reserve(t.Context(), 60, 0)
	if err != nil {
		t.Fatalf("first reservation failed: %v", err)
	}
	if got := observedState(t, bt, "run-reserve").ReservedUnits; got != 60 {
		t.Fatalf("ReservedUnits = %d, want 60", got)
	}
	if _, err := bt.Meter("run-reserve", limits).Reserve(t.Context(), 60, 0); err == nil {
		t.Fatal("second reservation unexpectedly succeeded despite insufficient remaining capacity")
	}
	if err := first.Settle(40, 0); err != nil {
		t.Fatalf("settle failed: %v", err)
	}
	state := observedState(t, bt, "run-reserve")
	if state.UnitsUsed != 40 || state.ReservedUnits != 0 {
		t.Fatalf("state after settle = %+v, want 40 used and no reservation", state)
	}
}

func TestBudgetConcurrentReservationsAreAtomic(t *testing.T) {
	bt := budget.NewTracker()
	limits := testBudget(100)
	const calls = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	reservations := make([]*budget.Reservation, 0, calls)
	for range calls {
		wg.Go(func() {
			reservation, err := bt.Meter("run-concurrent", limits).Reserve(t.Context(), 60, 0)
			if err != nil {
				return
			}
			mu.Lock()
			reservations = append(reservations, reservation)
			mu.Unlock()
		})
	}
	wg.Wait()
	if len(reservations) != 1 {
		t.Fatalf("successful reservations = %d, want exactly 1", len(reservations))
	}
	if err := reservations[0].Settle(60, 0); err != nil {
		t.Fatalf("settle failed: %v", err)
	}
	if got := observedState(t, bt, "run-concurrent").UnitsUsed; got != 60 {
		t.Errorf("UnitsUsed = %d, want 60", got)
	}
}

func TestBudgetReservationReleaseAllowsRetry(t *testing.T) {
	bt := budget.NewTracker()
	reservation, err := bt.Meter("run-release", testBudget(100)).Reserve(t.Context(), 100, 0)
	if err != nil {
		t.Fatalf("reserve failed: %v", err)
	}
	if err := reservation.Release(); err != nil {
		t.Fatalf("release failed: %v", err)
	}
	if _, err := bt.Meter("run-release", testBudget(100)).Reserve(t.Context(), 100, 0); err != nil {
		t.Fatalf("retry reservation failed after release: %v", err)
	}
}

func TestBudgetOldGenerationSettlementDoesNotOverwriteNewGeneration(t *testing.T) {
	bt := budget.NewTracker()
	old, err := bt.Meter("run-generation", testBudget(100)).Reserve(t.Context(), 80, 0)
	if err != nil {
		t.Fatalf("old-generation reserve failed: %v", err)
	}
	new, err := bt.Meter("run-generation", testBudget(100)).Reserve(t.Context(), 20, 0)
	if err != nil {
		t.Fatalf("new-generation reserve failed: %v", err)
	}
	if err := old.Settle(80, 0); err != nil {
		t.Fatalf("old-generation settle failed: %v", err)
	}
	if err := new.Settle(10, 0); err != nil {
		t.Fatalf("new-generation settle failed: %v", err)
	}
	state := observedState(t, bt, "run-generation")
	if state.UnitsUsed != 90 || state.ReservedUnits != 0 {
		t.Fatalf("new-generation state = %+v, want cumulative 90 used and no reservation", state)
	}
}

func TestBudgetPolicyTighteningUsesCumulativeUsage(t *testing.T) {
	bt := budget.NewTracker()
	r, err := bt.Meter("tighten", testBudget(100)).Reserve(t.Context(), 40, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Settle(40, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := bt.Meter("tighten", testBudget(50)).Reserve(t.Context(), 11, 0); err == nil {
		t.Fatal("expected tightened policy to reject usage beyond 50")
	}
}

func TestBudgetOverSettlementFailsClosed(t *testing.T) {
	bt := budget.NewTracker()
	r, err := bt.Meter("over", testBudget(100)).Reserve(t.Context(), 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Settle(21, 0); err == nil {
		t.Fatal("expected over-settlement error")
	}
	if got := observedState(t, bt, "over").UnitsUsed; got != 0 {
		t.Fatalf("over-settlement charged %d units", got)
	}
}
