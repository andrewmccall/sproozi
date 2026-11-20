package gateway

import (
	"context"
	"errors"

	"github.com/andrewmccall/sproozi/internal/budget"
)

// ErrRefusedBudget: the budget refused the request, because it is exhausted
// or because it has a monetary cap on an endpoint without pricing.
var ErrRefusedBudget = errors.New("gateway: budget refused the request")

// Spend charges one upstream request to the meter in ctx; a request without a
// meter has no configured budget and is not charged. The error matches
// ErrRefusedBudget or ErrBudgetUnavailable and keeps the budget's own text.
func Spend(ctx context.Context) error {
	err := budget.FromContext(ctx).SpendRequest(ctx)
	switch {
	case err == nil:
		return nil
	case budget.IsExhausted(err) || errors.Is(err, budget.ErrNoMonetaryPricing):
		return spendError{kind: ErrRefusedBudget, err: err}
	default:
		return spendError{kind: ErrBudgetUnavailable, err: err}
	}
}

// spendError classifies a budget error without changing its text.
type spendError struct{ kind, err error }

func (e spendError) Error() string   { return e.err.Error() }
func (e spendError) Unwrap() []error { return []error{e.kind, e.err} }
