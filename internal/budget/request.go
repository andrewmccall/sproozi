package budget

import (
	"context"
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

func (m *Meter) Reserve(units, costMicros int64) (*Reservation, error) {
	if m == nil {
		return nil, nil
	}
	r, err := m.tracker.ledger.Reserve(context.Background(), AccountKey(m.key), UsageReservation{
		MaxUnits: m.limits.MaxUnits, MaxCostMicros: m.limits.MaxCostMicros,
		Units: units, CostMicros: costMicros, RequireExisting: m.requireExisting,
	})
	if err != nil {
		return nil, err
	}
	return &Reservation{tracker: m.tracker, ledger: r}, nil
}

// Spend charges known usage atomically. Variable-cost requests instead reserve
// a conservative bound, then settle against trusted upstream usage.
func (m *Meter) Spend(units, costMicros int64) error {
	if m == nil {
		return nil
	}
	r, err := m.Reserve(units, costMicros)
	if err != nil {
		return err
	}
	return r.Settle(units, costMicros)
}

// SpendRequest charges a single upstream request. These endpoints have no
// trusted dollar pricing, so a monetary ceiling cannot be silently ignored.
func (m *Meter) SpendRequest() error {
	if m != nil && m.limits.MaxCostMicros > 0 {
		return fmt.Errorf("budget: endpoint has no trusted monetary pricing")
	}
	return m.Spend(1, 0)
}

func (m *Meter) State() (State, error) {
	if m == nil {
		return State{}, nil
	}
	if m.requireExisting {
		return m.tracker.ledger.State(context.Background(), AccountKey(m.key))
	}
	return m.tracker.State(m.key)
}
