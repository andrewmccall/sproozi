package profile

import (
	"errors"
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

// outcomeWants are the limits a launch asks for against each ceiling.
var outcomeWants = []session.Limit{{Bounded: true, Value: 0}, {Bounded: true, Value: 1}, {Bounded: true, Value: 5},
	{Bounded: true, Value: 6}, {}}

// outcomeCeilings are the fixed ceilings a launch's wants are measured against.
var outcomeCeilings = []session.Limit{{Bounded: true, Value: 0}, {Bounded: true, Value: 5}, {}}

// dimensionUnits names the units limit of an allowance in a recorded outcome.
const dimensionUnits = "units"

func outcomeError(err error) string {
	var rejection *Rejection
	switch {
	case err == nil:
		return "accepted"
	case errors.As(err, &rejection):
		return "rejected: " + string(rejection.Code)
	default:
		return "refused: " + err.Error()
	}
}

// The executor's limit rules answer as recorded in
// testdata/limit-outcomes.txt: what a launch on the wire, the fixed
// configuration and admission accept.
func TestLimitOutcomesMatchTheRecordedTable(t *testing.T) {
	t.Parallel()
	var lines []string
	for _, l := range outcomeLimits {
		units := session.Allowance{Units: l}
		cost := session.Allowance{Units: session.Limit{Bounded: true, Value: 5}, CostMicros: l}
		for _, c := range []struct {
			name string
			a    session.Allowance
		}{{dimensionUnits, units}, {"costMicros", cost}} {
			launch := launch(fixtureDigest)
			launch.Allowances = map[string]session.Allowance{testCapability: c.a}
			_, err := NewHandler(Config{Harness: launch.Harness, Principal: testPrincipal, Authorizer: tokenAuthorizer(testToken),
				Placement: launch.Placement, InstructionDigest: launch.InstructionDigest, Allowances: launch.Allowances,
				MaxCheckpointBytes: testMax, Backend: &retainedBackend{}})
			lines = append(lines,
				fmt.Sprintf("validateLaunch %s=%+v -> %s", c.name, l, outcomeError(validateLaunch(launch))),
				fmt.Sprintf("NewHandler %s=%+v -> %s", c.name, l, outcomeError(err)))
		}
	}
	for _, ceiling := range outcomeCeilings {
		for _, want := range outcomeWants {
			lines = append(lines, fmt.Sprintf("Limit.Fits want=%+v ceiling=%+v -> %t", want, ceiling, want.Fits(ceiling)))
			for _, dimension := range []string{dimensionUnits, "costMicros"} {
				fixed, asked := session.Allowance{Units: session.Limit{Bounded: true, Value: 5}}, session.Allowance{Units: session.Limit{Bounded: true, Value: 5}}
				if dimension == dimensionUnits {
					fixed.Units, asked.Units = ceiling, want
				} else {
					fixed.CostMicros, asked.CostMicros = ceiling, want
				}
				l := launch(fixtureDigest)
				l.Allowances = map[string]session.Allowance{testCapability: asked}
				h := &handler{config: Config{Harness: l.Harness, InstructionDigest: l.InstructionDigest,
					Allowances: map[string]session.Allowance{testCapability: fixed}}}
				lines = append(lines, fmt.Sprintf("validateAdmission %s want=%+v ceiling=%+v -> %s", dimension, want, ceiling,
					outcomeError(h.validateAdmission(actionPrepare, l))))
			}
		}
	}
	testutil.RecordedOutcomes(t, "testdata/limit-outcomes.txt", lines)
}
