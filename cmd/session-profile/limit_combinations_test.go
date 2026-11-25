package main

import (
	"fmt"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/testutil"
)

// modelCapability is the capability whose allowance the rows vary.
const modelCapability = "model.inference"

// The executor configuration's allowance check answers as recorded in
// testdata/limit-combinations.txt for an allowance's units and cost varied
// independently. The file uses only what the configuration had before its
// check called session.Limit, so the same rows can be produced there.
func TestLimitCombinationsMatchTheRecordedTable(t *testing.T) {
	t.Parallel()
	values := []session.Limit{{}, {Bounded: true, Value: 5}, {Bounded: true, Value: 0}, {Bounded: true, Value: -1},
		{Value: 1}, {Value: -1}}
	lines := make([]string, 0, len(values)*len(values))
	for _, units := range values {
		for _, cost := range values {
			config := validServerConfig()
			config.Allowances = map[string]session.Allowance{modelCapability: {Units: units, CostMicros: cost}}
			outcome := "accepted"
			if err := validateConfig(config); err != nil {
				outcome = "refused: " + err.Error()
			}
			lines = append(lines, fmt.Sprintf("validateConfig units=%+v costMicros=%+v -> %s", units, cost, outcome))
		}
	}
	testutil.RecordedOutcomes(t, "testdata/limit-combinations.txt", lines)
}
