package main

import (
	"testing"

	"github.com/andrewmccall/sproozi/internal/session"
)

// A configured limit is a positive bound or none (ADR 0051). A bound of 0 is
// refused with what to write instead: a positive bound, or no entry at all to
// forbid the capability.
func TestAConfiguredZeroBoundSaysWhatToDoInstead(t *testing.T) {
	t.Parallel()
	zero := session.Limit{Bounded: true, Value: 0}
	for _, c := range []struct {
		allowance session.Allowance
		want      string
	}{
		{session.Allowance{Units: zero},
			`configuration "allowances" entry "model.inference" has units bounded at 0; ` +
				`a bound must be positive, and leaving the entry out forbids its capability`},
		{session.Allowance{Units: session.Limit{Bounded: true, Value: 5}, CostMicros: zero},
			`configuration "allowances" entry "model.inference" has costMicros bounded at 0; ` +
				`a bound must be positive; for no cost cap write it unbounded with value 0, ` +
				`and to forbid the capability leave the entry out`},
	} {
		config := validServerConfig()
		config.Allowances = map[string]session.Allowance{modelCapability: c.allowance}
		if err := validateConfig(config); err == nil || err.Error() != c.want {
			t.Errorf("%+v: %v, want %s", c.allowance, err, c.want)
		}
	}
}
