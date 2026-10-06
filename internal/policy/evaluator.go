/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package policy provides pure admission evaluation for AgentRun requests.
package policy

import (
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/mcppolicy"
	packagepolicy "github.com/andrewmccall/sproozi/internal/packages"
)

// DenialReason is a stable, non-sensitive typed reason for an admission denial.
type DenialReason string

const (
	// DenialCapabilityNotAllowed means the run requests a capability the policy does not permit.
	DenialCapabilityNotAllowed DenialReason = "CapabilityNotAllowed"
	// DenialEgressProfileNotAllowed means the template selects an egress profile the policy does not permit.
	DenialEgressProfileNotAllowed DenialReason = "EgressProfileNotAllowed"
	// DenialResourceLimitExceeded means the runtime resource limit exceeds the policy bound.
	DenialResourceLimitExceeded DenialReason = "ResourceLimitExceeded"
	DenialPackageScopeInvalid   DenialReason = "PackageScopeInvalid"
	DenialMCPScopeInvalid       DenialReason = "MCPScopeInvalid"
)

// Denial carries the reason and a non-sensitive explanation for an admission denial.
type Denial struct {
	// Reason is the stable typed reason.
	Reason DenialReason
	// Message is a non-sensitive string safe to surface in status conditions.
	Message string
}

// Evaluate returns a non-nil Denial if the run, template, or runtime violates
// the policy, or nil if the run is permitted. All inputs are treated as read-only.
//
// Checks performed (in order):
//  1. Requested capabilities ⊆ AllowedCapabilities
//  2. Template egress profiles ⊆ policy EgressProfiles
//  3. Runtime resource limits ≤ policy ResourceBounds.Max (fails closed: a missing
//     limit for a policy-bounded resource is treated as unbounded and denied)
//  4. Runtime EphemeralWorkspace.SizeLimit ≤ ResourceBounds.Max[ephemeral-storage]
//
// Note: namespace and repository scoping are enforced by the respective gateways
// in Phases 5–7, not at controller admission time, because AgentRun and
// AgentTemplate do not carry target-namespace or repository fields.
func Evaluate(
	pol sprooziv1alpha1.AgentPolicySpec,
	tmpl sprooziv1alpha1.AgentTemplateSpec,
	run sprooziv1alpha1.AgentRunSpec,
	rt sprooziv1alpha1.AgentRuntimeSpec,
) *Denial {
	allowed := make(map[sprooziv1alpha1.CapabilityKind]struct{}, len(pol.AllowedCapabilities))
	for _, c := range pol.AllowedCapabilities {
		allowed[c] = struct{}{}
	}
	for _, c := range run.Capabilities {
		if _, ok := allowed[c]; !ok {
			return &Denial{
				Reason:  DenialCapabilityNotAllowed,
				Message: fmt.Sprintf("capability %q is not permitted by the policy", c),
			}
		}
		if name, mcp := c.MCPServerName(); mcp {
			if _, err := mcppolicy.CompileScope(pol.MCPServers[name]); err != nil {
				return &Denial{Reason: DenialMCPScopeInvalid, Message: "MCP tool scope is missing or invalid"}
			}
			if limits := pol.Budgets[c]; limits.MaxCostMicros > 0 {
				return &Denial{Reason: DenialMCPScopeInvalid, Message: "MCP capabilities have no trusted monetary pricing"}
			}
		}
	}
	if hasCapability(run.Capabilities, sprooziv1alpha1.CapabilityPackagesInstall) {
		if err := packagepolicy.ValidateScope(pol.PackagesInstall); err != nil {
			return &Denial{Reason: DenialPackageScopeInvalid, Message: "packages.install scope is invalid"}
		}
	}

	permittedProfiles := make(map[string]struct{}, len(pol.EgressProfiles))
	for _, p := range pol.EgressProfiles {
		permittedProfiles[p] = struct{}{}
	}
	for _, p := range tmpl.EgressProfiles {
		if _, ok := permittedProfiles[p]; !ok {
			return &Denial{
				Reason:  DenialEgressProfileNotAllowed,
				Message: fmt.Sprintf("egress profile %q is not permitted by the policy", p),
			}
		}
	}

	// Authorize the exact container limits that provisioning copies. Every
	// container, including administrator sidecars, consumes the Pod budget.
	for resourceName, maxQty := range pol.ResourceBounds.Max {
		total := resource.Quantity{}
		if len(rt.PodTemplate.Spec.Containers) == 0 {
			return &Denial{Reason: DenialResourceLimitExceeded, Message: "runtime has no containers"}
		}
		for _, container := range rt.PodTemplate.Spec.Containers {
			limit, ok := container.Resources.Limits[resourceName]
			if !ok || limit.Sign() <= 0 {
				return &Denial{Reason: DenialResourceLimitExceeded, Message: fmt.Sprintf("container %q has no positive limit for %q", container.Name, resourceName)}
			}
			total.Add(limit)
		}
		if total.Cmp(maxQty) > 0 {
			return &Denial{Reason: DenialResourceLimitExceeded, Message: fmt.Sprintf("combined container %q limit %s exceeds policy bound %s", resourceName, total.String(), maxQty.String())}
		}
	}

	// Check EphemeralWorkspace.SizeLimit against the ephemeral-storage bound (PRD §5
	// "storage limits"). An unset SizeLimit is treated as zero (no ephemeral workspace
	// configured), which is always within any finite bound.
	if maxEphemeral, ok := pol.ResourceBounds.Max[corev1.ResourceEphemeralStorage]; ok {
		wsSize := rt.EphemeralWorkspace.SizeLimit
		if !wsSize.IsZero() && wsSize.Cmp(maxEphemeral) > 0 {
			return &Denial{
				Reason:  DenialResourceLimitExceeded,
				Message: fmt.Sprintf("runtime ephemeral workspace size %s exceeds policy bound %s", wsSize.String(), maxEphemeral.String()),
			}
		}
	}

	return nil
}

func hasCapability(caps []sprooziv1alpha1.CapabilityKind, want sprooziv1alpha1.CapabilityKind) bool {
	return slices.Contains(caps, want)
}
