package gateway_test

import (
	"context"
	"errors"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

// Spend tells a budget that refused the request from one that could not be
// read, and keeps the budget's own text for the handlers that reply with it.
func TestSpendClassifiesBudgetErrorsAndKeepsTheirText(t *testing.T) {
	ctx := context.Background()
	exhausted := budget.NewTracker().Meter("exhausted", api.EndpointBudget{MaxUnits: 1})
	if err := exhausted.SpendRequest(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		meter *budget.Meter
		want  error
		text  string
	}{
		{"no budget", nil, nil, ""},
		{"exhausted", exhausted, gateway.ErrRefusedBudget, "budget exhausted: 1 units used, limit 1"},
		{"monetary cap without pricing", budget.NewTracker().Meter("cost", api.EndpointBudget{MaxUnits: 5, MaxCostMicros: 1}),
			gateway.ErrRefusedBudget, "budget: endpoint has no trusted monetary pricing"},
		{"ledger unavailable", budget.NewTrackerWithLedger(failingLedger{}).Meter("failing", api.EndpointBudget{MaxUnits: 5}),
			gateway.ErrBudgetUnavailable, errLedgerUnavailable.Error()},
	} {
		err := gateway.Spend(budget.WithMeter(ctx, tc.meter))
		if tc.want == nil {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		if !errors.Is(err, tc.want) || err.Error() != tc.text {
			t.Errorf("%s: Spend() = %v, want %v with text %q", tc.name, err, tc.want, tc.text)
		}
	}
}
