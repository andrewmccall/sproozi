package budget_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
)

// contextStore fails every call whose context is done, as an API client does.
type contextStore struct {
	mu      sync.Mutex
	records map[budget.AccountKey]*budget.Record
}

func (s *contextStore) Read(ctx context.Context, key budget.AccountKey) (*budget.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if record, ok := s.records[key]; ok {
		return record, nil
	}
	return nil, budget.ErrAccountNotFound
}

func (s *contextStore) Update(ctx context.Context, key budget.AccountKey, mutate func(*budget.Record, bool) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.records[key]
	if !exists {
		record = budget.NewRecord()
	}
	if err := mutate(record, exists); err != nil {
		return err
	}
	s.records[key] = record
	return nil
}

func TestMeterReserveObeysRequestDeadline(t *testing.T) {
	tracker := budget.NewTrackerWithLedger(budget.NewLedger(&contextStore{records: map[budget.AccountKey]*budget.Record{}}))
	meter := tracker.Meter("run/model", api.EndpointBudget{MaxUnits: 100})
	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()

	if _, err := meter.Reserve(expired, 10, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reserve after deadline: got %v, want DeadlineExceeded", err)
	}
	if _, err := meter.State(expired); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("State after deadline: got %v, want DeadlineExceeded", err)
	}
}

// A client disconnect after the upstream response must not lose settlement.
func TestReservationSettlesAfterRequestCancellation(t *testing.T) {
	tracker := budget.NewTrackerWithLedger(budget.NewLedger(&contextStore{records: map[budget.AccountKey]*budget.Record{}}))
	meter := tracker.Meter("run/model", api.EndpointBudget{MaxUnits: 100})
	request, cancel := context.WithCancel(t.Context())
	reservation, err := meter.Reserve(request, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	cancel()

	if err := reservation.Settle(7, 0); err != nil {
		t.Fatalf("Settle after request cancellation: %v", err)
	}
	if state := observedMeterState(t, meter); state.UnitsUsed != 7 || state.ReservedUnits != 0 {
		t.Fatalf("state after settlement = %+v, want 7 used and nothing reserved", state)
	}
}

func TestOptionalBudgetAndUnpricedCost(t *testing.T) {
	var absent *budget.Meter
	if err := absent.SpendRequest(t.Context()); err != nil {
		t.Fatal(err)
	}
	tracker := budget.NewTracker()
	if err := tracker.Meter("run/github", api.EndpointBudget{MaxCostMicros: 1}).SpendRequest(t.Context()); err == nil {
		t.Fatal("unpriced endpoint accepted a cost ceiling")
	}
}

func TestCostOnlyBudgetCanSettleUnboundedUnits(t *testing.T) {
	meter := budget.NewTracker().Meter("run/model", api.EndpointBudget{MaxCostMicros: 100})
	r, err := meter.Reserve(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Settle(123, 70); err != nil {
		t.Fatal(err)
	}
	if state := observedMeterState(t, meter); state.UnitsUsed != 123 || state.CostMicros != 70 || state.ReservedCostMicros != 0 {
		t.Fatalf("unexpected settlement: %+v", state)
	}
}

func TestInitializedMeterRequiresAccountForObservationAndSpend(t *testing.T) {
	ledger := budget.NewMemoryLedger()
	tracker := budget.NewTrackerWithLedger(ledger)
	meter := tracker.InitializedMeter("placement/model", api.EndpointBudget{MaxUnits: 10})
	if _, err := meter.State(t.Context()); !errors.Is(err, budget.ErrAccountNotFound) {
		t.Fatalf("missing initialized observation: %v", err)
	}
	if err := meter.SpendRequest(t.Context()); err == nil {
		t.Fatal("missing initialized account admitted usage")
	}
	if err := tracker.Initialize("placement/model"); err != nil {
		t.Fatal(err)
	}
	if err := meter.SpendRequest(t.Context()); err != nil {
		t.Fatal(err)
	}
	if state, err := meter.State(t.Context()); err != nil || state.UnitsUsed != 1 {
		t.Fatalf("initialized usage: %+v %v", state, err)
	}
	if _, err := ledger.Seal(t.Context(), "placement/model"); err != nil {
		t.Fatal(err)
	}
	if err := meter.SpendRequest(t.Context()); err == nil {
		t.Fatal("sealed allocation admitted usage")
	}
}
