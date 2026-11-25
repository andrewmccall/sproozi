package session

import (
	"math"
	"testing"

	"github.com/andrewmccall/sproozi/internal/budget"
)

// otherCapability is a second capability name for receipts that disagree.
const otherCapability = "other.capability"

// CheckUsage answers every receipt as the checks it replaced in remote
// work's export (internal/work/export.go) and the Docker gateway's retained
// record (internal/gateway/docker/owner.go) did. Both earlier checks, copied
// here as they were, compared the lengths first and then each allocation.
func TestCheckUsageAnswersAsTheReceiptChecksItReplaced(t *testing.T) {
	t.Parallel()
	export := func(allowances map[string]Allowance, usage map[string]budget.TrustedUsage) bool {
		if len(usage) != len(allowances) {
			return false
		}
		for cap, a := range allowances {
			u, ok := usage[cap]
			if !ok || u.Units < 0 || u.CostMicros < 0 || a.Units.Bounded && u.Units > a.Units.Value || a.CostMicros.Bounded && u.CostMicros > a.CostMicros.Value {
				return false
			}
		}
		return true
	}
	gateway := func(allowances map[string]Allowance, receipt map[string]budget.TrustedUsage) bool {
		if len(receipt) != len(allowances) {
			return false
		}
		for cap, a := range allowances {
			u, ok := receipt[cap]
			if !ok || u.Units < 0 || u.CostMicros < 0 || (a.Units.Bounded && u.Units > a.Units.Value) || (a.CostMicros.Bounded && u.CostMicros > a.CostMicros.Value) {
				return false
			}
		}
		return true
	}
	bounded := Allowance{Units: Limit{Bounded: true, Value: 5}, CostMicros: Limit{Bounded: true, Value: 7}}
	exhausted := Allowance{Units: Limit{Bounded: true, Value: 0}, CostMicros: Limit{Bounded: true, Value: 0}}
	one := func(a Allowance) map[string]Allowance { return map[string]Allowance{testCapability: a} }
	used := func(units, cost int64) map[string]budget.TrustedUsage {
		return map[string]budget.TrustedUsage{testCapability: {Units: units, CostMicros: cost}}
	}
	for _, c := range []struct {
		name       string
		allowances map[string]Allowance
		usage      map[string]budget.TrustedUsage
		want       bool
	}{
		{"both empty", map[string]Allowance{}, map[string]budget.TrustedUsage{}, true},
		{"usage names an extra capability", one(bounded), map[string]budget.TrustedUsage{testCapability: {}, otherCapability: {}}, false},
		{"usage misses an allocated capability", map[string]Allowance{testCapability: bounded, otherCapability: bounded}, used(0, 0), false},
		{"same length, different capability", one(bounded), map[string]budget.TrustedUsage{otherCapability: {}}, false},
		{"no usage", one(bounded), used(0, 0), true},
		{"usage equal to both bounds", one(bounded), used(5, 7), true},
		{"units above the bound", one(bounded), used(6, 7), false},
		{"cost above the bound", one(bounded), used(5, 8), false},
		{"negative units", one(bounded), used(-1, 0), false},
		{"negative cost", one(bounded), used(0, -1), false},
		{"unbounded, any usage", one(Allowance{}), used(math.MaxInt64, math.MaxInt64), true},
		{"unbounded, negative usage", one(Allowance{}), used(-1, 0), false},
		{"exhausted, no usage", one(exhausted), used(0, 0), true},
		{"exhausted, one unit", one(exhausted), used(1, 0), false},
		{"exhausted, one micro", one(exhausted), used(0, 1), false},
	} {
		got := CheckUsage(c.allowances, c.usage) == nil
		if got != c.want || export(c.allowances, c.usage) != c.want || gateway(c.allowances, c.usage) != c.want {
			t.Errorf("%s: CheckUsage=%t export=%t gateway=%t, want %t", c.name, got,
				export(c.allowances, c.usage), gateway(c.allowances, c.usage), c.want)
		}
	}
}
