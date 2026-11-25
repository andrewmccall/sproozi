package placement

import (
	"errors"
	"fmt"
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

// The fixed configuration and admission checks answer as recorded in
// testdata/limit-combinations.txt for an allowance's units and cost varied
// independently. The file uses only what the placement had before its
// checks called session.Limit, so the same rows can be produced there.
func TestLimitCombinationsMatchTheRecordedTable(t *testing.T) {
	t.Parallel()
	values := []session.Limit{{}, {Bounded: true, Value: 5}, {Bounded: true, Value: 0}, {Bounded: true, Value: -1}, {Value: 1}, {Value: -1}}
	answer := func(err error) string {
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
	config := func(a session.Allowance) Config {
		return Config{Harness: harness.NativeCodex, Kube: fake.NewClientBuilder().Build(), RESTConfig: &rest.Config{Host: "https://combinations.invalid"},
			Namespace: fixtureNamespace, BudgetNamespace: DefaultBudgetNamespace, AgentNamespace: runtimekube.AgentsNamespace,
			TemplateName: fixtureTemplate, Principal: "executor-principal", Name: fixtureRemote, Directory: "/state/profile",
			InstructionDigest: registry.InstructionsDigest(fixtureInstruction), MaxCheckpointBytes: fixtureMax,
			Allowances: map[string]session.Allowance{string(api.CapabilityModelInference): a}}
	}
	policy := api.AgentPolicySpec{AllowedCapabilities: []api.CapabilityKind{api.CapabilityModelInference}}
	lines := make([]string, 0, 3*len(values)*len(values))
	for _, units := range values {
		for _, cost := range values {
			a := session.Allowance{Units: units, CostMicros: cost}
			lines = append(lines, fmt.Sprintf("validateConfig units=%+v costMicros=%+v -> %s", units, cost, answer(validateConfig(config(a)))))
			// Fixed ceilings that every want fits, and ones it may not.
			for _, fixed := range []session.Allowance{{}, {Units: session.Limit{Bounded: true, Value: 5}, CostMicros: session.Limit{Bounded: true, Value: 5}}} {
				p := &Placement{config: config(fixed)}
				_, err := p.ceilings(session.Launch{Allowances: map[string]session.Allowance{string(api.CapabilityModelInference): a}}, policy)
				lines = append(lines, fmt.Sprintf("ceilings units=%+v costMicros=%+v fixed=%+v -> %s", units, cost, fixed, answer(err)))
			}
		}
	}
	testutil.RecordedOutcomes(t, "testdata/limit-combinations.txt", lines)
}
