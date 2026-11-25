package home

import (
	"fmt"
	"math"
	"strings"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/testutil"
)

// outcomeLimits are the limit values every layer's recorded outcomes cover:
// a bounded zero, positive and negative bounds, the largest bound, and an
// unbounded limit with a zero, positive and negative value.
var outcomeLimits = []session.Limit{{Bounded: true, Value: 0}, {Bounded: true, Value: 1}, {Bounded: true, Value: 5},
	{Bounded: true, Value: -1}, {Bounded: true, Value: math.MaxInt64}, {}, {Value: 1}, {Value: -1}}

// outcomeWants are the limits a retained execution holds against each ceiling.
var outcomeWants = []session.Limit{{Bounded: true, Value: 0}, {Bounded: true, Value: 1}, {Bounded: true, Value: 5},
	{Bounded: true, Value: 6}, {}}

// outcomeCeilings are the Project ceilings a retained execution is measured against.
var outcomeCeilings = []session.Limit{{Bounded: true, Value: 0}, {Bounded: true, Value: 5}, {}}

// The home's limit rules answer as recorded in testdata/limit-outcomes.txt:
// which configured allowances it names as problems, the approved profile it
// composes from them, and which retained limits fit a Project's ceiling.
func TestLimitOutcomesMatchTheRecordedTable(t *testing.T) {
	t.Parallel()
	base := composedConfig(t)
	var lines []string
	for _, capability := range []string{testCapability, "mcp.docs"} {
		for _, l := range outcomeLimits {
			for _, c := range []struct {
				name string
				a    session.Allowance
			}{{"units", session.Allowance{Units: l}}, {"costMicros", session.Allowance{Units: session.Limit{Bounded: true, Value: 5}, CostMicros: l}}} {
				config := base
				config.Allowances = map[string]session.Allowance{capability: c.a}
				problems := "no problems"
				if p := config.AllowanceProblems(); len(p) != 0 {
					problems = strings.Join(p, "; ")
				}
				policy, err := approvedPolicy(config)
				approved := fmt.Sprintf("accepted %+v", policy.Budgets[api.CapabilityKind(capability)])
				if err != nil {
					approved = "refused: " + err.Error()
				}
				lines = append(lines,
					fmt.Sprintf("AllowanceProblems %s %s=%+v -> %s", capability, c.name, l, problems),
					fmt.Sprintf("approvedPolicy %s %s=%+v -> %s", capability, c.name, l, approved))
			}
		}
	}
	for _, ceiling := range outcomeCeilings {
		for _, want := range outcomeWants {
			lines = append(lines, fmt.Sprintf("Limit.Fits want=%+v ceiling=%+v -> %t", want, ceiling, want.Fits(ceiling)))
		}
	}
	testutil.RecordedOutcomes(t, "testdata/limit-outcomes.txt", lines)
}
