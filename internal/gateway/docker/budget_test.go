package docker_test

import (
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	gatewaydocker "github.com/andrewmccall/sproozi/internal/gateway/docker"
	"github.com/andrewmccall/sproozi/internal/session"
)

// A session without a model budget runs with no unit or cost limit; only
// model inference may go without one (ADR 0051).
func TestModelInferenceMayRunWithoutABudget(t *testing.T) {
	f := newOwner(t)
	if err := f.owner.Close(); err != nil {
		t.Fatal(err)
	}
	f.config.Policy.Spec.Budgets[api.CapabilityModelInference] = api.EndpointBudget{}
	f.launch.Allowances[modelCapability] = session.Allowance{}
	var err error
	if f.owner, err = gatewaydocker.Open(f.config); err != nil {
		t.Fatal(err)
	}
	token := f.initialized(t)
	if err = f.owner.Activate(t.Context(), f.launch, f.instance); err != nil {
		t.Fatal(err)
	}
	grant, err := f.owner.Authenticate(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	limits, configured, err := grant.BudgetLimits(api.CapabilityModelInference)
	if err != nil || !configured || limits.MaxUnits != 0 || limits.MaxCostMicros != 0 {
		t.Fatalf("limits %+v configured %v err %v; want none", limits, configured, err)
	}
	if err = f.owner.Revoke(t.Context(), f.launch, f.instance); err != nil {
		t.Fatal(err)
	}
	usage, err := f.owner.Seal(t.Context(), f.launch, f.instance)
	if err != nil || usage[modelCapability].Units != 0 {
		t.Fatalf("seal %+v err %v", usage, err)
	}
}

// A launch without a unit limit is refused under a policy that has one, and
// for any capability other than model inference.
func TestUnlimitedAllowanceNeedsUnlimitedModelPolicy(t *testing.T) {
	f := newOwner(t)
	f.launch.Allowances[modelCapability] = session.Allowance{}
	if _, err := f.owner.Initialize(t.Context(), f.launch, f.instance); err == nil {
		t.Fatal("an unlimited allowance passed a policy with a unit ceiling")
	}
	if err := f.owner.Close(); err != nil {
		t.Fatal(err)
	}
	native := api.CapabilityKubernetesRead
	f.config.NativeCapabilities = []api.CapabilityKind{native}
	f.config.Policy.Spec.AllowedCapabilities = append(f.config.Policy.Spec.AllowedCapabilities, native)
	f.config.Policy.Spec.Budgets[native] = api.EndpointBudget{}
	if _, err := gatewaydocker.Open(f.config); err == nil {
		t.Fatal("a native capability opened without a unit ceiling")
	}
}
