package placement

import (
	"errors"
	"fmt"
	"math"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/harness"
	runtimekube "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/testutil"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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

func outcomeError(err error) string {
	var rejection *profile.Rejection
	switch {
	case err == nil:
		return "accepted"
	case errors.As(err, &rejection):
		return "rejected: " + string(rejection.Code)
	default:
		return "refused: " + err.Error()
	}
}

func outcomeConfig(allowance session.Allowance) Config {
	return Config{Harness: harness.NativeCodex, Kube: fake.NewClientBuilder().Build(), RESTConfig: &rest.Config{Host: "https://fixture.invalid"},
		Namespace: fixtureNamespace, BudgetNamespace: DefaultBudgetNamespace, AgentNamespace: runtimekube.AgentsNamespace,
		TemplateName: fixtureTemplate, Principal: "executor-principal", Name: fixtureRemote, Directory: "/state/profile",
		InstructionDigest: registry.InstructionsDigest(fixtureInstruction), MaxCheckpointBytes: fixtureMax,
		Allowances: map[string]session.Allowance{string(api.CapabilityModelInference): allowance}}
}

// The Kubernetes placement's limit rules answer as recorded in
// testdata/limit-outcomes.txt: what its fixed configuration accepts, and
// which launch allowances it admits under each fixed ceiling.
func TestLimitOutcomesMatchTheRecordedTable(t *testing.T) {
	t.Parallel()
	lines := make([]string, 0, 256)
	for _, l := range outcomeLimits {
		units := session.Allowance{Units: l}
		cost := session.Allowance{Units: session.Limit{Bounded: true, Value: 5}, CostMicros: l}
		lines = append(lines,
			fmt.Sprintf("validateConfig units=%+v -> %s", l, outcomeError(validateConfig(outcomeConfig(units)))),
			fmt.Sprintf("validateConfig costMicros=%+v -> %s", l, outcomeError(validateConfig(outcomeConfig(cost)))))
	}
	policy := api.AgentPolicySpec{AllowedCapabilities: []api.CapabilityKind{api.CapabilityModelInference}}
	for _, ceiling := range outcomeCeilings {
		for _, want := range outcomeWants {
			lines = append(lines, fmt.Sprintf("Limit.Fits want=%+v ceiling=%+v -> %t", want, ceiling, want.Fits(ceiling)))
			for _, dimension := range []string{"units", "costMicros"} {
				fixed, asked := session.Allowance{Units: session.Limit{Bounded: true, Value: 5}}, session.Allowance{Units: session.Limit{Bounded: true, Value: 5}}
				if dimension == "units" {
					fixed.Units, asked.Units = ceiling, want
				} else {
					fixed.CostMicros, asked.CostMicros = ceiling, want
				}
				p := &Placement{config: outcomeConfig(fixed)}
				ceilings, err := p.ceilings(session.Launch{Allowances: map[string]session.Allowance{string(api.CapabilityModelInference): asked}}, policy)
				outcome := outcomeError(err)
				if err == nil {
					outcome = fmt.Sprintf("%s %+v", outcome, ceilings[api.CapabilityModelInference])
				}
				lines = append(lines, fmt.Sprintf("ceilings %s want=%+v ceiling=%+v -> %s", dimension, want, ceiling, outcome))
			}
		}
	}
	testutil.RecordedOutcomes(t, "testdata/limit-outcomes.txt", lines)
}
