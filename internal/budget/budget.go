package budget

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
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
	// Recovery, when set, keeps the reservation as a lease the ledger can
	// settle if its gateway never does.
	Recovery *Recovery
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

// ErrUsageExceeded reports that trusted usage exceeded its reservation or the
// allowance. Settlement still recorded the usage, capped at the allowance, and
// the reservation is settled; callers must not release or forfeit it.
var ErrUsageExceeded = errors.New("budget: settled usage exceeds reservation or budget")

type ErrExhausted struct{ UnitsUsed, MaxUnits int64 }

func (e *ErrExhausted) Error() string {
	return fmt.Sprintf("budget exhausted: %d units used, limit %d", e.UnitsUsed, e.MaxUnits)
}

// ErrCostExhausted reports that the cost allowance cannot cover a reservation.
type ErrCostExhausted struct{ CostMicros, MaxCostMicros int64 }

func (e *ErrCostExhausted) Error() string {
	return fmt.Sprintf("budget: cost budget exhausted (%d of %d micros used)", e.CostMicros, e.MaxCostMicros)
}

// IsExhausted reports whether err refused a reservation because the units or
// cost left in the allowance cannot cover it.
func IsExhausted(err error) bool {
	var units *ErrExhausted
	var cost *ErrCostExhausted
	return errors.As(err, &units) || errors.As(err, &cost)
}

// LedgerReservation is an opaque admission held by one request.
type LedgerReservation struct {
	mu                      sync.Mutex
	runID                   AccountKey
	units, costMicros       int64
	maxUnits, maxCostMicros int64
	settled                 bool
	// lease names the reservation's durable lease, if it is recoverable.
	lease string
}

func (r *LedgerReservation) unbounded() bool {
	return r.maxUnits == 0 || r.maxCostMicros == 0
}

// settlementTimeout bounds one settlement, release or forfeit.
const settlementTimeout = 30 * time.Second

// Reservation is capacity held for one request until it settles.
type Reservation struct {
	tracker *Tracker
	ledger  *LedgerReservation
	// settleCtx is the reserving request's context without its cancellation.
	// Settlement follows the upstream response, so a client disconnect must
	// not cancel it: a lost settlement leaves capacity reserved or usage
	// unrecorded. settlementTimeout bounds it instead.
	settleCtx context.Context
}

// Held returns the units and cost the reservation holds against limited
// dimensions. Zero means that dimension has no limit.
func (r *Reservation) Held() (units, costMicros int64) {
	if r == nil {
		return 0, 0
	}
	if r.ledger.maxUnits > 0 {
		units = r.ledger.units
	}
	if r.ledger.maxCostMicros > 0 {
		costMicros = r.ledger.costMicros
	}
	return units, costMicros
}

func (r *Reservation) Settle(units, cost int64) error {
	if r == nil {
		return fmt.Errorf("budget: nil budget reservation")
	}
	ctx, cancel := context.WithTimeout(r.settleCtx, settlementTimeout)
	defer cancel()
	return r.tracker.ledger.Settle(ctx, r.ledger, TrustedUsage{Units: units, CostMicros: cost})
}
func (r *Reservation) Release() error {
	if r == nil {
		return fmt.Errorf("budget: nil budget reservation")
	}
	ctx, cancel := context.WithTimeout(r.settleCtx, settlementTimeout)
	defer cancel()
	return r.tracker.ledger.Release(ctx, r.ledger)
}

// Forfeit charges the reserved units and cost of each limited dimension when
// upstream consumption cannot be verified. A dimension with no limit charges
// nothing: its reservation is only the request's own bound, not consumption.
// The ledger atomically consumes the bounds and rejects repeats.
func (r *Reservation) Forfeit() error {
	if r == nil {
		return fmt.Errorf("budget: nil budget reservation")
	}
	units, cost := r.Held()
	return r.Settle(units, cost)
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
