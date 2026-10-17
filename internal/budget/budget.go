package budget

import (
	"context"
	"errors"
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
	// RequireExisting is set for initialized placement allocations. Losing the
	// record must deny subsequent usage rather than recreate an empty balance.
	RequireExisting bool
}

// TrustedUsage is usage returned by a trusted provider adapter.
type TrustedUsage struct {
	Units      int64
	CostMicros int64
}

// Ledger is the durable atomic accounting seam. Implementations must
// atomically reserve, settle, and release against the account key.
type Ledger interface {
	// Initialize establishes an expected account before execution activation.
	// Sealing requires that record to still exist; deletion is not fresh usage.
	Initialize(context.Context, AccountKey) error
	Reserve(context.Context, AccountKey, UsageReservation) (*LedgerReservation, error)
	Settle(context.Context, *LedgerReservation, TrustedUsage) error
	Release(context.Context, *LedgerReservation) error
	// State returns ErrAccountNotFound for a confirmed absent record, and an
	// error for unavailable or corrupt accounting. Tracker alone maps ordinary
	// unused accounts to zero; managed meters require the record.
	State(context.Context, AccountKey) (State, error)
	// Seal atomically prevents new reservations and retains the used plus
	// outstanding reserved usage as an immutable receipt for this account.
	// Pending unbounded usage and unknown legacy reservation history cannot seal.
	Seal(context.Context, AccountKey) (TrustedUsage, error)
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

func (r *LedgerReservation) unbounded() bool {
	return r.maxUnits == 0 || r.maxCostMicros == 0
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

type Tracker struct {
	ledger         Ledger
	requests       sync.Mutex
	activeRequests map[string]*requestSlot
}

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
func (bt *Tracker) State(runID string) (State, error) {
	state, err := bt.ledger.State(context.Background(), AccountKey(runID))
	if errors.Is(err, ErrAccountNotFound) {
		return State{}, nil
	}
	return state, err
}

func (bt *Tracker) Initialize(runID string) error {
	return bt.ledger.Initialize(context.Background(), AccountKey(runID))
}

func (bt *Tracker) Seal(runID string) (TrustedUsage, error) {
	return bt.ledger.Seal(context.Background(), AccountKey(runID))
}
