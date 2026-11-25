package docker

import (
	"fmt"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/testutil"
)

// The Docker placement's launch check answers as recorded in
// testdata/limit-combinations.txt for an allowance's units and cost varied
// independently. The file uses only what the placement had before its check
// called session.Limit, so the same rows can be produced there.
func TestLimitCombinationsMatchTheRecordedTable(t *testing.T) {
	t.Parallel()
	values := []session.Limit{{}, {Bounded: true, Value: 5}, {Bounded: true, Value: 0}, {Bounded: true, Value: -1}, {Value: 1}, {Value: -1}}
	base := fixtureLaunch(t, fixtureArtifact(t))
	p := &Placement{config: fixtureConfig()}
	lines := make([]string, 0, len(values)*len(values))
	for _, units := range values {
		for _, cost := range values {
			launch := base
			launch.Allowances = map[string]session.Allowance{modelFixtureCapability: {Units: units, CostMicros: cost}}
			outcome := "accepted"
			if err := p.validateLaunch(launch); err != nil {
				outcome = "refused: " + err.Error()
			}
			lines = append(lines, fmt.Sprintf("validateLaunch units=%+v costMicros=%+v -> %s", units, cost, outcome))
		}
	}
	testutil.RecordedOutcomes(t, "testdata/limit-combinations.txt", lines)
}
