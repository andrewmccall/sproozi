package session

import (
	"fmt"
	"math"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/testutil"
)

// outcomeLimits are the limit values every layer's recorded outcomes cover:
// a bounded zero, positive and negative bounds, the largest bound, and an
// unbounded limit with a zero, positive and negative value.
var outcomeLimits = []Limit{{Bounded: true, Value: 0}, {Bounded: true, Value: 1}, {Bounded: true, Value: 5},
	{Bounded: true, Value: -1}, {Bounded: true, Value: math.MaxInt64}, {}, {Value: 1}, {Value: -1}}

func outcomeError(err error) string {
	if err == nil {
		return "accepted"
	}
	return "refused: " + err.Error()
}

// The session layer's limit rules answer as recorded in
// testdata/limit-outcomes.txt.
func TestLimitOutcomesMatchTheRecordedTable(t *testing.T) {
	t.Parallel()
	lines := make([]string, 0, 256)
	for _, l := range outcomeLimits {
		lines = append(lines, fmt.Sprintf("Limit.Valid %+v -> %t", l, l.Valid()))
	}
	for _, capability := range []api.CapabilityKind{api.CapabilityModelInference, "mcp.docs"} {
		for _, l := range outcomeLimits {
			units := Allowance{Units: l}
			cost := Allowance{Units: Limit{Bounded: true, Value: 5}, CostMicros: l}
			lines = append(lines, fmt.Sprintf("Allowance.Valid %s units=%+v -> %t", capability, l, units.Valid(capability)),
				fmt.Sprintf("Allowance.Valid %s costMicros=%+v -> %t", capability, l, cost.Valid(capability)))
		}
	}
	for _, l := range outcomeLimits {
		units := map[string]Allowance{testCapability: {Units: l}}
		cost := map[string]Allowance{testCapability: {Units: Limit{Bounded: true, Value: 5}, CostMicros: l}}
		lines = append(lines,
			fmt.Sprintf("validateAllowance units=%+v -> %s", l, outcomeError(validateAllowance(units))),
			fmt.Sprintf("validateAllowance costMicros=%+v -> %s", l, outcomeError(validateAllowance(cost))),
			fmt.Sprintf("spendable units=%+v -> %s", l, outcomeError(spendable(units))),
			fmt.Sprintf("spendable costMicros=%+v -> %s", l, outcomeError(spendable(cost))))
		for _, used := range []int64{0, 1, 5, 6} {
			usage := map[string]budget.TrustedUsage{testCapability: {Units: used, CostMicros: used}}
			lines = append(lines,
				fmt.Sprintf("CheckUsage units=%+v used=%d -> %s", l, used, outcomeError(CheckUsage(units, usage))),
				fmt.Sprintf("CheckUsage costMicros=%+v used=%d -> %s", l, used,
					outcomeError(CheckUsage(map[string]Allowance{testCapability: {CostMicros: l}},
						map[string]budget.TrustedUsage{testCapability: {CostMicros: used}}))))
		}
	}
	testutil.RecordedOutcomes(t, "testdata/limit-outcomes.txt", lines)
}
