package home

import (
	"slices"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session"
)

// A configured limit is a positive bound or none (ADR 0051). A bound of 0 is
// refused with what to write instead: a positive bound, or no allowance at
// all to forbid the capability.
func TestAConfiguredZeroBoundSaysWhatToDoInstead(t *testing.T) {
	t.Parallel()
	zero := session.Limit{Bounded: true, Value: 0}
	positive := session.Limit{Bounded: true, Value: 5}
	for _, c := range []struct {
		capability string
		allowance  session.Allowance
		want       string
	}{
		{testCapability, session.Allowance{Units: zero},
			`allowances.model.inference.units ("bounded": true needs a positive "value"; leave allowances.model.inference out to forbid its capability)`},
		{testCapability, session.Allowance{Units: positive, CostMicros: zero},
			`allowances.model.inference.costMicros ("bounded": true needs a positive "value"; leave allowances.model.inference out to forbid its capability)`},
		{"mcp.search", session.Allowance{Units: zero},
			`allowances.mcp.search.units ("bounded": true needs a positive "value"; leave allowances.mcp.search out to forbid its capability)`},
	} {
		config := Config{Allowances: map[string]session.Allowance{c.capability: c.allowance}}
		if problems := config.AllowanceProblems(); !slices.Contains(problems, c.want) {
			t.Errorf("%s %+v: problems %q, want %q among them", c.capability, c.allowance, problems, c.want)
		}
	}
}
