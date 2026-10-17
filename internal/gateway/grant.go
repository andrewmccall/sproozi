package gateway

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
)

var ErrTokenInvalid = errors.New("gateway: token invalid or expired")
var ErrGrantRevoked = errors.New("gateway: execution grant revoked")
var ErrBudgetUnavailable = errors.New("gateway: budget accounting unavailable")

const (
	BackendKubernetes = "kubernetes"
	BackendDocker     = "docker"
)

// Reference identifies the execution or authenticated subject in its owning
// backend. UID is immutable; names alone do not establish authority.
type Reference struct {
	Backend, Namespace, Name, UID string
}

type PolicySnapshot struct {
	Name, UID  string
	Generation int64
	Spec       api.AgentPolicySpec
}

type TemplateSnapshot struct {
	Name, UID  string
	Generation int64
	Spec       api.AgentTemplateSpec
}

// Allocation is issued by the execution owner, never supplied by the workload.
// A zero monetary ceiling is unbounded; zero units cannot express an allocation.
type Allocation struct {
	AccountKey string
	Ceiling    api.EndpointBudget
	Exhausted  bool
}

// ExecutionGrant contains resolved authorization snapshots, without requiring
// a Kubernetes AgentRun for other execution backends.
type ExecutionGrant struct {
	Execution    Reference
	Authority    Reference
	Policy       PolicySnapshot
	Template     TemplateSnapshot
	Capabilities []api.CapabilityKind
	Managed      bool
	Allocations  map[api.CapabilityKind]Allocation
}

func (g *ExecutionGrant) Validate() error {
	if g == nil || g.Execution.Backend == "" || g.Execution.UID == "" || g.Policy.Name == "" || g.Template.Name == "" {
		return ErrTokenInvalid
	}
	if g.Managed {
		for _, cap := range g.Capabilities {
			a, ok := g.Allocations[cap]
			if !ok || a.AccountKey == "" || a.Ceiling.MaxUnits <= 0 || a.Ceiling.MaxCostMicros < 0 {
				return ErrTokenInvalid
			}
		}
	}
	return nil
}

// Permits reports whether both the execution and its live policy grant the
// capability. The Dispatcher checks it for fixed routes; MCP checks it once the
// server name has selected the capability.
func (g *ExecutionGrant) Permits(capability api.CapabilityKind) bool {
	return slices.Contains(g.Capabilities, capability) && slices.Contains(g.Policy.Spec.AllowedCapabilities, capability)
}

// Clone freezes administrator-owned input before the transport retains it.
func (g *ExecutionGrant) Clone() *ExecutionGrant {
	if g == nil {
		return nil
	}
	out := *g
	out.Policy.Spec = *g.Policy.Spec.DeepCopy()
	out.Template.Spec = *g.Template.Spec.DeepCopy()
	out.Capabilities = slices.Clone(g.Capabilities)
	out.Allocations = maps.Clone(g.Allocations)
	return &out
}

// BudgetLimits intersects current policy with the placement's issued ceiling.
// Exhaustion and missing managed allocations are explicit denials, rather than
// the zero values used for ordinary unbounded budgets.
func (g *ExecutionGrant) BudgetLimits(cap api.CapabilityKind) (api.EndpointBudget, bool, error) {
	limits, configured := g.Policy.Spec.Budgets[cap]
	if !g.Managed {
		return limits, configured, nil
	}
	a, ok := g.Allocations[cap]
	if !ok || a.AccountKey == "" || a.Ceiling.MaxUnits <= 0 || a.Ceiling.MaxCostMicros < 0 || a.Exhausted {
		return api.EndpointBudget{}, false, fmt.Errorf("gateway: allocation denied")
	}
	limits.MaxUnits = tighter(limits.MaxUnits, a.Ceiling.MaxUnits)
	limits.MaxCostMicros = tighter(limits.MaxCostMicros, a.Ceiling.MaxCostMicros)
	return limits, true, nil
}

func tighter(policy, allocation int64) int64 {
	if policy <= 0 {
		return allocation
	}
	if allocation <= 0 || policy < allocation {
		return policy
	}
	return allocation
}

// Meter selects the account and limits together for every capability entrypoint.
func (g *ExecutionGrant) Meter(tracker *budget.Tracker, cap api.CapabilityKind) (*budget.Meter, error) {
	limits, configured, err := g.BudgetLimits(cap)
	if err != nil || !configured {
		return nil, err
	}
	if tracker == nil {
		return nil, ErrBudgetUnavailable
	}
	if g.Managed {
		meter := tracker.InitializedMeter(g.Allocations[cap].AccountKey, limits)
		if _, err := meter.State(); err != nil {
			return nil, err
		}
		return meter, nil
	}
	return tracker.Meter(g.Execution.UID+"/"+string(cap), limits), nil
}
