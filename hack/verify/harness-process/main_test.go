package main

import (
	"testing"

	"github.com/andrewmccall/sproozi/internal/budget"
)

func TestAccountingDistinguishesAbsentFromConsumed(t *testing.T) {
	ledger := budget.NewMemoryLedger()
	out, err := observeAccounting(result{Scenario: deniedScenario}, ledger)
	if err != nil || out.UnitsUsed != nil || out.ReservedUnits != nil || out.ToolUnitsUsed != nil {
		t.Fatalf("Denied work manufactured accounting: %+v %v", out, err)
	}
	for _, input := range []result{
		{Scenario: successScenario},
		{Scenario: deniedScenario, ModelRequests: 1},
		{Scenario: deniedScenario, ToolCalls: 1},
	} {
		if _, err := observeAccounting(input, ledger); err == nil {
			t.Fatalf("Accepted missing accounting after work: %+v", input)
		}
	}
}
