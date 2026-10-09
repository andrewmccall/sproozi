package budget

import (
	"context"
	"fmt"
	"sync"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

// AccountKey combines the immutable Run UID and capability to partition usage.
type AccountKey string

// UsageReservation describes a conservative admission request. Limits are
// supplied on every call so tightening a policy takes effect immediately.
type UsageReservation struct {
	MaxUnits      int64
	MaxCostMicros int64
	Units         int64
	CostMicros    int64
}

// TrustedUsage is usage returned by a trusted provider adapter.
type TrustedUsage struct {
	Units      int64
	CostMicros int64
}

// Ledger is the durable atomic accounting seam. Implementations must
// atomically reserve, settle, and release against the account key.
type Ledger interface {
	Reserve(context.Context, AccountKey, UsageReservation) (*LedgerReservation, error)
	Settle(context.Context, *LedgerReservation, TrustedUsage) error
	Release(context.Context, *LedgerReservation) error
	State(context.Context, AccountKey) State
}

type State struct {
	UnitsUsed, CostMicros             int64
	ReservedUnits, ReservedCostMicros int64
}

type ErrExhausted struct{ UnitsUsed, MaxUnits int64 }

func (e *ErrExhausted) Error() string {
	return fmt.Sprintf("budget exhausted: %d units used, limit %d", e.UnitsUsed, e.MaxUnits)
}

// LedgerReservation is an opaque admission held by one request.
type LedgerReservation struct {
	mu                      sync.Mutex
	runID                   AccountKey
	units, costMicros       int64
	maxUnits, maxCostMicros int64
	settled                 bool
}

// MemoryLedger is a concurrency-safe ledger. It is deliberately
// injectable and shared between gateway replicas in tests; production can
// replace it with a transactional Kubernetes/SQL adapter without changing
// admission or settlement semantics.
type MemoryLedger struct {
	mu    sync.Mutex
	state map[AccountKey]State
}

func NewMemoryLedger() *MemoryLedger {
	return &MemoryLedger{state: make(map[AccountKey]State)}
}

func (l *MemoryLedger) Reserve(_ context.Context, runID AccountKey, req UsageReservation) (*LedgerReservation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.state[runID]
	r, err := reserveState(&s, req)
	if err != nil {
		return nil, err
	}
	r.runID = runID
	l.state[runID] = s
	return r, nil
}

func (l *MemoryLedger) Settle(_ context.Context, r *LedgerReservation, usage TrustedUsage) error {
	if r == nil {
		return fmt.Errorf("budget: nil budget reservation")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.state[r.runID]
	if err := settleState(&s, r, usage); err != nil {
		return err
	}
	l.state[r.runID] = s
	r.settled = true
	return nil
}

func (l *MemoryLedger) Release(ctx context.Context, r *LedgerReservation) error {
	return l.Settle(ctx, r, TrustedUsage{})
}
func (l *MemoryLedger) State(_ context.Context, runID AccountKey) State {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state[runID]
}

type Reservation struct {
	tracker *Tracker
	ledger  *LedgerReservation
}

func (r *Reservation) Settle(units, cost int64) error {
	if r == nil {
		return fmt.Errorf("budget: nil budget reservation")
	}
	return r.tracker.ledger.Settle(context.Background(), r.ledger, TrustedUsage{Units: units, CostMicros: cost})
}
func (r *Reservation) Release() error {
	if r == nil {
		return fmt.Errorf("budget: nil budget reservation")
	}
	return r.tracker.ledger.Release(context.Background(), r.ledger)
}

// Forfeit charges all reserved units and cost when upstream consumption cannot
// be verified. The ledger atomically consumes both bounds and rejects repeats.
func (r *Reservation) Forfeit() error {
	if r == nil {
		return fmt.Errorf("budget: nil budget reservation")
	}
	return r.Settle(r.ledger.units, r.ledger.costMicros)
}

func (r *Reservation) ReservedUnits() int64 {
	if r == nil || r.ledger == nil {
		return 0
	}
	return r.ledger.units
}

type Tracker struct{ ledger Ledger }

func NewTracker() *Tracker { return NewTrackerWithLedger(NewMemoryLedger()) }
func NewTrackerWithLedger(ledger Ledger) *Tracker {
	return &Tracker{ledger: ledger}
}
func (bt *Tracker) Reserve(runID string, budget sprooziv1alpha1.EndpointBudget, units, cost int64) (*Reservation, error) {
	r, err := bt.ledger.Reserve(context.Background(), AccountKey(runID), UsageReservation{MaxUnits: budget.MaxUnits, MaxCostMicros: budget.MaxCostMicros, Units: units, CostMicros: cost})
	if err != nil {
		return nil, err
	}
	return &Reservation{tracker: bt, ledger: r}, nil
}
func (bt *Tracker) State(runID string) State {
	s := bt.ledger.State(context.Background(), AccountKey(runID))
	return s
}
