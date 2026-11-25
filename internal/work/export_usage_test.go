package work

import (
	"maps"
	"testing"

	"github.com/google/uuid"

	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
)

// An export carries the receipt of its writer's last visit, and a receipt
// that used more than its allowance, or that does not name exactly the
// allocated capabilities, is not evidence of a stop: ValidateExport refuses
// it as unavailable, and accepts one that used exactly its bound.
func TestExportReceiptMustFitItsAllowances(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	o, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	v := submit(t, o)
	waitPhase(t, o, v.ID, phaseRetained)
	exported, err := o.BeginExport(t.Context(), v.ID, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateExport(exported, c.MaxCheckpointBytes); err != nil {
		t.Fatalf("the export as sealed: %v", err)
	}
	const other = "github.pull_request"
	bounded := func(units, cost int64) session.Allowance {
		return session.Allowance{Units: session.Limit{Bounded: true, Value: units}, CostMicros: session.Limit{Bounded: true, Value: cost}}
	}
	// with returns the export whose launch, and the launch its receipt
	// repeats, carry allowances, and whose receipt carries usage.
	with := func(allowances map[string]session.Allowance, usage map[string]budget.TrustedUsage) Export {
		e := exported
		receipt := *e.Stopped.Receipt
		e.Stopped.Launch.Allowances = maps.Clone(allowances)
		receipt.Launch.Allowances = maps.Clone(allowances)
		receipt.Usage = maps.Clone(usage)
		e.Stopped.Receipt = &receipt
		return e
	}
	model := map[string]session.Allowance{fixtureModel: bounded(5, 50)}
	use := func(units, cost int64) map[string]budget.TrustedUsage {
		return map[string]budget.TrustedUsage{fixtureModel: {Units: units, CostMicros: cost}}
	}
	for _, tc := range []struct {
		name   string
		export Export
		ok     bool
	}{
		{"units at the bound", with(model, use(5, 0)), true},
		{"units one over the bound", with(model, use(6, 0)), false},
		{"cost at the bound", with(model, use(0, 50)), true},
		{"cost one over the bound", with(model, use(0, 51)), false},
		{"a capability the allowances do not have", with(model, map[string]budget.TrustedUsage{other: {}}), false},
		{"a capability the receipt misses", with(map[string]session.Allowance{fixtureModel: bounded(5, 50), other: bounded(5, 50)}, use(0, 0)), false},
	} {
		err := ValidateExport(tc.export, c.MaxCheckpointBytes)
		switch {
		case tc.ok && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case !tc.ok && (err == nil || OutcomeCode(err) != profile.CodeUnavailable):
			t.Errorf("%s: got %v, want an unavailable rejection", tc.name, err)
		}
	}
}
