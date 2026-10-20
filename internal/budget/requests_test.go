package budget_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
	budgetfile "github.com/andrewmccall/sproozi/internal/budget/file"
	"github.com/andrewmccall/sproozi/internal/testutil"
)

type admission struct {
	reservation *budget.Reservation
	end         func()
	err         error
}

func admit(ctx context.Context, meter *budget.Meter, units int64, bounded bool) <-chan admission {
	done := make(chan admission, 1)
	go func() {
		reservation, end, err := meter.ReserveRequest(ctx, units, 0, bounded, nil)
		done <- admission{reservation, end, err}
	}()
	return done
}

func admitted(t *testing.T, done <-chan admission) admission {
	t.Helper()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		return result
	case <-time.After(time.Second):
		t.Fatal("call was not admitted")
	}
	return admission{}
}

func waiting(t *testing.T, done <-chan admission) {
	t.Helper()
	select {
	case result := <-done:
		t.Fatalf("call was admitted while it should wait: %+v", result)
	case <-time.After(50 * time.Millisecond):
	}
}

func finish(t *testing.T, call admission, units int64) {
	t.Helper()
	if err := call.reservation.Settle(units, 0); err != nil {
		t.Fatal(err)
	}
	call.end()
}

func TestRequestWaitingIsCancellableAndAccountsAreIndependent(t *testing.T) {
	tracker := budget.NewTracker()
	first := tracker.Meter("first/model", api.EndpointBudget{MaxUnits: 100})
	held := admitted(t, admit(t.Context(), first, 0, false))
	other := admitted(t, admit(t.Context(), tracker.Meter("other/model", api.EndpointBudget{MaxUnits: 100}), 0, false))
	finish(t, other, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, _, err := tracker.Meter("first/model", api.EndpointBudget{MaxUnits: 100}).ReserveRequest(ctx, 0, 0, false, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued cancellation: %v", err)
	}
	finish(t, held, 1)
	finish(t, admitted(t, admit(t.Context(), first, 0, false)), 1)
}

func TestBoundedCallsRunTogetherWhileTheyFit(t *testing.T) {
	tracker := budget.NewTracker()
	meter := tracker.Meter("run/model", api.EndpointBudget{MaxUnits: 100})
	first := admitted(t, admit(t.Context(), meter, 40, true))
	second := admitted(t, admit(t.Context(), meter, 40, true))
	third := admit(t.Context(), meter, 40, true)
	waiting(t, third)
	finish(t, first, 10)
	resumed := admitted(t, third)
	finish(t, second, 10)
	finish(t, resumed, 10)
	state, err := tracker.State("run/model")
	if err != nil || state.UnitsUsed != 30 || state.ReservedUnits != 0 {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

// A bounded call larger than what remains reserves the remainder once nothing
// else is pending, as an unbounded call does, and is refused only at zero.
func TestBoundedCallThatCannotFitReservesTheRemainderAlone(t *testing.T) {
	tracker := budget.NewTracker()
	meter := tracker.Meter("run/model", api.EndpointBudget{MaxUnits: 100})
	call := admitted(t, admit(t.Context(), meter, 500, true))
	if units, _ := call.reservation.Held(); units != 100 {
		t.Fatalf("held %d units, want the remaining 100", units)
	}
	later := admit(t.Context(), meter, 10, true)
	waiting(t, later)
	finish(t, call, 100)
	result := <-later
	if result.err == nil || !budget.IsExhausted(result.err) {
		t.Fatalf("exhausted account admitted a call: %+v", result)
	}
}

func TestUnboundedCallRunsAlone(t *testing.T) {
	tracker := budget.NewTracker()
	meter := tracker.Meter("run/model", api.EndpointBudget{MaxUnits: 100})
	bounded := admitted(t, admit(t.Context(), meter, 10, true))
	unbounded := admit(t.Context(), meter, 0, false)
	waiting(t, unbounded)
	// A bounded call that arrives after the unbounded one waits for it.
	later := admit(t.Context(), meter, 10, true)
	waiting(t, later)
	finish(t, bounded, 5)
	exclusive := admitted(t, unbounded)
	if units, _ := exclusive.reservation.Held(); units != 95 {
		t.Fatalf("unbounded call held %d units, want the remaining 95", units)
	}
	waiting(t, later)
	finish(t, exclusive, 5)
	finish(t, admitted(t, later), 5)
}

// An exceeding call may take only the capacity no other pending call holds,
// so a parallel call that stays within its reservation is not exceeded and
// settled usage never passes the allowance.
func TestExceedingParallelCallLeavesOtherReservations(t *testing.T) {
	tracker := budget.NewTracker()
	meter := tracker.Meter("run/model", api.EndpointBudget{MaxUnits: 1000, MaxCostMicros: 1000})
	reserve := func() *budget.Reservation {
		reservation, end, err := meter.ReserveRequest(t.Context(), 400, 400, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(end)
		return reservation
	}
	a, b := reserve(), reserve()
	if err := a.Settle(1000, 1000); !errors.Is(err, budget.ErrUsageExceeded) {
		t.Fatalf("exceeding call settled with %v", err)
	}
	if err := b.Settle(218, 218); err != nil {
		t.Fatalf("call within its reservation settled with %v", err)
	}
	state, err := tracker.State("run/model")
	if err != nil || state.UnitsUsed != 818 || state.CostMicros != 818 || state.ReservedUnits != 0 {
		t.Fatalf("state=%+v err=%v, want 600 + 218 used", state, err)
	}
}

// A gateway that stops without settling leaves its reservation as a lease.
// Once the lease expires, the next reservation or seal settles it at its
// recovery charge, capped at what it reserved: never a refund, never held
// for good. A late settlement by the original gateway then changes nothing.
func TestExpiredReservationIsSettledAtItsRecoveryCharge(t *testing.T) {
	ledger := budget.NewMemoryLedger()
	tracker := budget.NewTrackerWithLedger(ledger)
	meter := tracker.InitializedMeter("run/model", api.EndpointBudget{MaxUnits: 1000})
	if err := tracker.Initialize("run/model"); err != nil {
		t.Fatal(err)
	}
	abandoned, _, err := meter.ReserveRequest(t.Context(), 0, 0, false,
		&budget.Recovery{ExpiresAt: time.Now().Add(-time.Second), Units: 300})
	if err != nil {
		t.Fatal(err)
	}
	// The unbounded reservation holds everything, so sealing cannot finish
	// until it is recovered; recovering it is what sealing does first.
	if receipt, err := ledger.Seal(t.Context(), "run/model"); err != nil || receipt.Units != 300 {
		t.Fatalf("seal after an abandoned reservation = %+v, %v", receipt, err)
	}
	state, err := tracker.State("run/model")
	if err != nil || state.UnitsUsed != 300 || state.ReservedUnits != 0 {
		t.Fatalf("state=%+v err=%v, want the recovery charge of 300", state, err)
	}
	if err := abandoned.Settle(5, 0); !errors.Is(err, budget.ErrReservationRecovered) {
		t.Fatalf("late settlement = %v", err)
	}
}

// A lease that has not expired keeps its reservation.
func TestUnexpiredReservationIsNotRecovered(t *testing.T) {
	tracker := budget.NewTracker()
	meter := tracker.Meter("run/model", api.EndpointBudget{MaxUnits: 1000})
	held, end, err := meter.ReserveRequest(t.Context(), 100, 0, true,
		&budget.Recovery{ExpiresAt: time.Now().Add(time.Hour), Units: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer end()
	other, endOther, err := meter.ReserveRequest(t.Context(), 100, 0, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer endOther()
	if err := other.Settle(1, 0); err != nil {
		t.Fatal(err)
	}
	if err := held.Settle(7, 0); err != nil {
		t.Fatal(err)
	}
	if state, _ := tracker.State("run/model"); state.UnitsUsed != 8 || state.ReservedUnits != 0 {
		t.Fatalf("state=%+v", state)
	}
}

// Recovery commits on its own: a reservation that then fails for lack of
// budget does not discard it, and the recovery is logged once.
func TestRecoveryPersistsWhenTheFollowingReservationFails(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	tracker := budget.NewTracker()
	meter := tracker.Meter("run/model", api.EndpointBudget{MaxUnits: 1000})
	if _, _, err := meter.ReserveRequest(t.Context(), 0, 0, false,
		&budget.Recovery{ExpiresAt: time.Now().Add(-time.Second), Units: 300, Model: true}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := meter.Reserve(t.Context(), 800, 0); !budget.IsExhausted(err) {
			t.Fatalf("reserve after recovery = %v, want exhausted", err)
		}
	}
	state, err := tracker.State("run/model")
	if err != nil || state.UnitsUsed != 300 || state.ReservedUnits != 0 {
		t.Fatalf("state=%+v err=%v, want the recovery kept", state, err)
	}
	if got := strings.Count(logs.String(), "Settled abandoned model reservation"); got != 1 {
		t.Fatalf("recovery logged %d times: %s", got, logs.String())
	}
}

// A second settlement of one reservation says so, rather than claiming the
// lease was recovered.
func TestSecondSettlementIsAlreadySettled(t *testing.T) {
	tracker := budget.NewTracker()
	reservation, err := tracker.Meter("run/model", api.EndpointBudget{MaxUnits: 100}).Reserve(t.Context(), 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Settle(5, 0); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Settle(5, 0); !errors.Is(err, budget.ErrAlreadySettled) {
		t.Fatalf("second settlement = %v", err)
	}
}

// Every reservation has a lease, including one that is not a model call, so
// a gateway's startup sweep never takes another gateway's live reservation
// for a legacy one; once its request deadline has passed, it is recovered in
// full and logged as a budget reservation.
func TestRequestReservationsAreLeased(t *testing.T) {
	ledger, err := budgetfile.NewLedger(filepath.Join(testutil.PrivateDir(t), "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ledger.Close() }()
	tracker := budget.NewTrackerWithLedger(ledger)
	ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()
	if _, err := tracker.Meter("run/github", api.EndpointBudget{MaxUnits: 100}).Reserve(ctx, 1, 0); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Sweep(time.Now(), true); err != nil {
		t.Fatal(err)
	}
	if state, _ := tracker.State("run/github"); state.UnitsUsed != 0 || state.ReservedUnits != 1 {
		t.Fatalf("a live leased reservation was taken for a legacy one: %+v", state)
	}
	if err := ledger.Sweep(time.Now().Add(2*time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if state, _ := tracker.State("run/github"); state.UnitsUsed != 1 || state.ReservedUnits != 0 {
		t.Fatalf("an expired request lease was not recovered in full: %+v", state)
	}
}
