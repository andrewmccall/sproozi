package budget

import (
	"context"
	"errors"
	"fmt"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

// Meter binds the live limits and durable accounting key for one request.
// A nil meter means this capability has no configured budget.
type Meter struct {
	tracker         *Tracker
	key             string
	limits          sprooziv1alpha1.EndpointBudget
	requireExisting bool
}

type contextKey struct{}

func WithMeter(ctx context.Context, meter *Meter) context.Context {
	return context.WithValue(ctx, contextKey{}, meter)
}

func FromContext(ctx context.Context) *Meter {
	meter, _ := ctx.Value(contextKey{}).(*Meter)
	return meter
}

func (bt *Tracker) Meter(key string, limits sprooziv1alpha1.EndpointBudget) *Meter {
	return &Meter{tracker: bt, key: key, limits: limits}
}

// InitializedMeter binds a managed placement to its pre-created account. A
// deleted account is unavailable, never a newly unused allowance.
func (bt *Tracker) InitializedMeter(key string, limits sprooziv1alpha1.EndpointBudget) *Meter {
	return &Meter{tracker: bt, key: key, limits: limits, requireExisting: true}
}

// Reserve holds capacity for one request. ctx bounds the reservation itself;
// the returned Reservation settles under ctx's values but not its cancellation.
func (m *Meter) Reserve(ctx context.Context, units, costMicros int64) (*Reservation, error) {
	return m.reserve(ctx, units, costMicros, nil)
}

// reserve holds capacity under a lease, so the ledger can recover it if this
// gateway stops before settling. Without a model call's recovery, the lease
// charges the reservation in full once the request's deadline has passed.
// Every reservation therefore has a lease, and a pending reservation without
// one can only come from a gateway from before leases.
func (m *Meter) reserve(ctx context.Context, units, costMicros int64, recovery *Recovery) (*Reservation, error) {
	if m == nil {
		return nil, nil
	}
	request := UsageReservation{MaxUnits: m.limits.MaxUnits, MaxCostMicros: m.limits.MaxCostMicros,
		Units: units, CostMicros: costMicros, RequireExisting: m.requireExisting, Recovery: recovery}
	if request.Recovery == nil {
		request.Recovery = requestRecovery(ctx, units, costMicros, request)
	}
	r, err := m.tracker.ledger.Reserve(ctx, AccountKey(m.key), request)
	if err != nil {
		return nil, err
	}
	return &Reservation{tracker: m.tracker, ledger: r, settleCtx: context.WithoutCancel(ctx)}, nil
}

// Spend charges known usage atomically. Variable-cost requests instead reserve
// a conservative bound, then settle against trusted upstream usage.
func (m *Meter) Spend(ctx context.Context, units, costMicros int64) error {
	if m == nil {
		return nil
	}
	r, err := m.Reserve(ctx, units, costMicros)
	if err != nil {
		return err
	}
	return r.Settle(units, costMicros)
}

// SpendRequest charges a single upstream request. These endpoints have no
// trusted dollar pricing, so a monetary ceiling cannot be silently ignored.
func (m *Meter) SpendRequest(ctx context.Context) error {
	if m != nil && m.limits.MaxCostMicros > 0 {
		return fmt.Errorf("budget: endpoint has no trusted monetary pricing")
	}
	return m.Spend(ctx, 1, 0)
}

// State reads the account. An ordinary meter treats an absent account as
// unused; an initialized meter requires the record.
func (m *Meter) State(ctx context.Context) (State, error) {
	if m == nil {
		return State{}, nil
	}
	state, err := m.tracker.ledger.State(ctx, AccountKey(m.key))
	if !m.requireExisting && errors.Is(err, ErrAccountNotFound) {
		return State{}, nil
	}
	return state, err
}
