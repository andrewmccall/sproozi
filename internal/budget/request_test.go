package budget_test

import (
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
)

func TestOptionalBudgetAndUnpricedCost(t *testing.T) {
	var absent *budget.Meter
	if err := absent.SpendRequest(); err != nil {
		t.Fatal(err)
	}
	tracker := budget.NewTracker()
	if err := tracker.Meter("run/github", api.EndpointBudget{MaxCostMicros: 1}).SpendRequest(); err == nil {
		t.Fatal("unpriced endpoint accepted a cost ceiling")
	}
}

func TestCostOnlyBudgetCanSettleUnboundedUnits(t *testing.T) {
	meter := budget.NewTracker().Meter("run/model", api.EndpointBudget{MaxCostMicros: 100})
	r, err := meter.Reserve(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Settle(123, 70); err != nil {
		t.Fatal(err)
	}
	if state := meter.State(); state.UnitsUsed != 123 || state.CostMicros != 70 || state.ReservedCostMicros != 0 {
		t.Fatalf("unexpected settlement: %+v", state)
	}
}
