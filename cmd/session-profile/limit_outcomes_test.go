package main

import (
	"fmt"
	"math"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/testutil"
)

// outcomeLimits are the limit values every layer's recorded outcomes cover:
// a bounded zero, positive and negative bounds, the largest bound, and an
// unbounded limit with a zero, positive and negative value.
var outcomeLimits = []session.Limit{{Bounded: true, Value: 0}, {Bounded: true, Value: 1}, {Bounded: true, Value: 5},
	{Bounded: true, Value: -1}, {Bounded: true, Value: math.MaxInt64}, {}, {Value: 1}, {Value: -1}}

// The executor configuration's limit rule answers as recorded in
// testdata/limit-outcomes.txt.
func TestLimitOutcomesMatchTheRecordedTable(t *testing.T) {
	t.Parallel()
	var lines []string
	for _, l := range outcomeLimits {
		for _, c := range []struct {
			name string
			a    session.Allowance
		}{
			{"units", session.Allowance{Units: l}},
			{"costMicros", session.Allowance{Units: session.Limit{Bounded: true, Value: 5}, CostMicros: l}},
		} {
			config := validServerConfig()
			config.Allowances = map[string]session.Allowance{"model.inference": c.a}
			outcome := "accepted"
			if err := validateConfig(config); err != nil {
				outcome = "refused: " + err.Error()
			}
			lines = append(lines, fmt.Sprintf("validateConfig %s=%+v -> %s", c.name, l, outcome))
		}
	}
	testutil.RecordedOutcomes(t, "testdata/limit-outcomes.txt", lines)
}
