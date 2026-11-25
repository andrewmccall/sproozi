package docker

import (
	"fmt"
	"math"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/testutil"
)

// outcomeLimits are the limit values every layer's recorded outcomes cover:
// a bounded zero, positive and negative bounds, the largest bound, and an
// unbounded limit with a zero, positive and negative value.
var outcomeLimits = []session.Limit{{Bounded: true, Value: 0}, {Bounded: true, Value: 1}, {Bounded: true, Value: 5},
	{Bounded: true, Value: -1}, {Bounded: true, Value: math.MaxInt64}, {}, {Value: 1}, {Value: -1}}

// The Docker gateway's limit rules answer as recorded in
// testdata/limit-outcomes.txt: which launch allocations it accepts under an
// approved profile with and without budgets, and the limits an accepted
// allocation then meters with.
func TestLimitOutcomesMatchTheRecordedTable(t *testing.T) {
	t.Parallel()
	const mcpDocs = api.CapabilityKind("mcp.docs")
	var lines []string
	for _, budgets := range []map[api.CapabilityKind]api.EndpointBudget{nil, {
		api.CapabilityModelInference: {MaxUnits: 5, MaxCostMicros: 5}, mcpDocs: {MaxUnits: 5}}} {
		owner := &Owner{policy: gateway.PolicySnapshot{Spec: api.AgentPolicySpec{
			AllowedCapabilities: []api.CapabilityKind{api.CapabilityModelInference, mcpDocs}, Budgets: budgets}}}
		for _, capability := range []api.CapabilityKind{api.CapabilityModelInference, mcpDocs} {
			for _, l := range outcomeLimits {
				for _, c := range []struct {
					name string
					a    session.Allowance
				}{{"units", session.Allowance{Units: l}}, {"costMicros", session.Allowance{Units: session.Limit{Bounded: true, Value: 5}, CostMicros: l}}} {
					lines = append(lines, fmt.Sprintf("validateAllocation budgets=%t %s %s=%+v -> %s",
						budgets != nil, capability, c.name, l, allocationOutcome(owner, capability, c.a)))
				}
			}
		}
	}
	testutil.RecordedOutcomes(t, "testdata/limit-outcomes.txt", lines)
}

// allocationOutcome is whether owner accepts a as capability's allocation
// and, if it does, the limits its grant meters with.
func allocationOutcome(owner *Owner, capability api.CapabilityKind, a session.Allowance) string {
	if err := owner.validateAllocation(string(capability), a); err != nil {
		return "refused: " + err.Error()
	}
	g := owner.grant(record{Launch: session.Launch{AllocationID: "allocation", Allowances: map[string]session.Allowance{string(capability): a}}})
	limits, _, err := g.BudgetLimits(capability)
	if err != nil {
		return "accepted; meters: " + err.Error()
	}
	return fmt.Sprintf("accepted; meters %+v", limits)
}
