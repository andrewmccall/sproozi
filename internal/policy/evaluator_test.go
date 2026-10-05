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

package policy_test

import (
	"testing"
	"time"

	"k8s.io/utils/ptr"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/policy"
)

const (
	testWorkloadContainer = "agent"
)

const ephemeralStorageMax = "4Gi"

const egressGoModules = "go-modules"

func basePolicy() sprooziv1alpha1.AgentPolicySpec {
	return sprooziv1alpha1.AgentPolicySpec{
		AllowedCapabilities: []sprooziv1alpha1.CapabilityKind{
			sprooziv1alpha1.CapabilityKubernetesRead,
			sprooziv1alpha1.CapabilityGitHubPullRequest,
			sprooziv1alpha1.CapabilityModelInference,
		},
		EgressProfiles: []string{egressGoModules, "pypi"},
		ResourceBounds: sprooziv1alpha1.ResourceBounds{Max: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("2"),
			corev1.ResourceMemory:           resource.MustParse("2Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse(ephemeralStorageMax),
		}},
		MaxExecutionDuration: metav1.Duration{Duration: time.Hour},
	}
}

func baseTemplate(egressProfiles ...string) sprooziv1alpha1.AgentTemplateSpec {
	return sprooziv1alpha1.AgentTemplateSpec{
		EgressProfiles: egressProfiles,
	}
}

func baseRun(caps ...sprooziv1alpha1.CapabilityKind) sprooziv1alpha1.AgentRunSpec {
	return sprooziv1alpha1.AgentRunSpec{
		Capabilities: caps,
	}
}

func baseRuntime() sprooziv1alpha1.AgentRuntimeSpec {
	return sprooziv1alpha1.AgentRuntimeSpec{

		EphemeralWorkspace: sprooziv1alpha1.EphemeralWorkspaceSpec{SizeLimit: resource.MustParse("2Gi")}, PodTemplate: corev1.
					PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: testWorkloadContainer, Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("1"),
				corev1.ResourceMemory:           resource.MustParse("1Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
			},
		}}}, SecurityContext: &corev1.
			PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)),
			FSGroup: ptr.To(int64(65532)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}}}, GatewayEndpoint: "https://sproozi-gateway.sproozi-system.svc:8443",
	}
}

func TestEvaluateCapabilities(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		policy     sprooziv1alpha1.AgentPolicySpec
		run        sprooziv1alpha1.AgentRunSpec
		wantDenied bool
		wantReason policy.DenialReason
	}{
		{
			name:       "all requested capabilities allowed",
			policy:     basePolicy(),
			run:        baseRun(sprooziv1alpha1.CapabilityKubernetesRead, sprooziv1alpha1.CapabilityGitHubPullRequest),
			wantDenied: false,
		},
		{
			name:       "no capabilities requested",
			policy:     basePolicy(),
			run:        baseRun(),
			wantDenied: false,
		},
		{
			name:       "single capability not in allowed list",
			policy:     basePolicy(),
			run:        baseRun(sprooziv1alpha1.CapabilityNetworkEgress),
			wantDenied: true,
			wantReason: policy.DenialCapabilityNotAllowed,
		},
		{
			name:       "one of multiple capabilities not allowed",
			policy:     basePolicy(),
			run:        baseRun(sprooziv1alpha1.CapabilityKubernetesRead, sprooziv1alpha1.CapabilityNetworkEgress),
			wantDenied: true,
			wantReason: policy.DenialCapabilityNotAllowed,
		},
		{
			name: "policy permits no capabilities, run requests none",
			policy: sprooziv1alpha1.AgentPolicySpec{
				AllowedCapabilities: []sprooziv1alpha1.CapabilityKind{},
				EgressProfiles:      []string{},
			},
			run:        baseRun(),
			wantDenied: false,
		},
		{
			name: "policy permits no capabilities, run requests one",
			policy: sprooziv1alpha1.AgentPolicySpec{
				AllowedCapabilities: []sprooziv1alpha1.CapabilityKind{},
				EgressProfiles:      []string{},
			},
			run:        baseRun(sprooziv1alpha1.CapabilityKubernetesRead),
			wantDenied: true,
			wantReason: policy.DenialCapabilityNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Use a template with no egress profiles to isolate capability checks.
			denial := policy.Evaluate(tt.policy, baseTemplate(), tt.run, baseRuntime())
			if tt.wantDenied && denial == nil {
				t.Fatal("Evaluate() = nil, want denial")
			}
			if !tt.wantDenied && denial != nil {
				t.Fatalf("Evaluate() = %+v, want nil", denial)
			}
			if tt.wantDenied && denial.Reason != tt.wantReason {
				t.Errorf("Evaluate().Reason = %q, want %q", denial.Reason, tt.wantReason)
			}
		})
	}
}

func TestEvaluateEgressProfiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		policyProfiles []string
		tmplProfiles   []string
		wantDenied     bool
		wantReason     policy.DenialReason
	}{
		{
			name:           "template profiles subset of policy profiles",
			policyProfiles: []string{egressGoModules, "pypi"},
			tmplProfiles:   []string{egressGoModules},
			wantDenied:     false,
		},
		{
			name:           "template requests no egress profiles",
			policyProfiles: []string{egressGoModules},
			tmplProfiles:   []string{},
			wantDenied:     false,
		},
		{
			name:           "template profile not in policy",
			policyProfiles: []string{egressGoModules},
			tmplProfiles:   []string{"npm"},
			wantDenied:     true,
			wantReason:     policy.DenialEgressProfileNotAllowed,
		},
		{
			name:           "one of multiple template profiles not in policy",
			policyProfiles: []string{egressGoModules},
			tmplProfiles:   []string{egressGoModules, "npm"},
			wantDenied:     true,
			wantReason:     policy.DenialEgressProfileNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := sprooziv1alpha1.AgentPolicySpec{
				AllowedCapabilities: basePolicy().AllowedCapabilities,
				EgressProfiles:      tt.policyProfiles,
			}
			denial := policy.Evaluate(p, baseTemplate(tt.tmplProfiles...), baseRun(), baseRuntime())
			if tt.wantDenied && denial == nil {
				t.Fatal("Evaluate() = nil, want denial")
			}
			if !tt.wantDenied && denial != nil {
				t.Fatalf("Evaluate() = %+v, want nil", denial)
			}
			if tt.wantDenied && denial.Reason != tt.wantReason {
				t.Errorf("Evaluate().Reason = %q, want %q", denial.Reason, tt.wantReason)
			}
		})
	}
}

func TestEvaluateResourceLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		policyMax     corev1.ResourceList
		runtimeLimits corev1.ResourceList
		wantDenied    bool
		wantReason    policy.DenialReason
	}{
		{
			name:          "runtime within policy bounds",
			policyMax:     corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			runtimeLimits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			wantDenied:    false,
		},
		{
			name:          "runtime equals policy bound",
			policyMax:     corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			runtimeLimits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			wantDenied:    false,
		},
		{
			name:          "runtime exceeds policy CPU bound",
			policyMax:     corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			runtimeLimits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			wantDenied:    true,
			wantReason:    policy.DenialResourceLimitExceeded,
		},
		{
			name:          "runtime exceeds policy memory bound",
			policyMax:     corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
			runtimeLimits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
			wantDenied:    true,
			wantReason:    policy.DenialResourceLimitExceeded,
		},
		{
			name:          "policy has no bounds, any runtime allowed",
			policyMax:     corev1.ResourceList{},
			runtimeLimits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100")},
			wantDenied:    false,
		},
		{
			// No limit = unbounded = exceeds any finite policy bound (fail closed).
			name:          "runtime has no limit for bounded resource — denied",
			policyMax:     corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			runtimeLimits: corev1.ResourceList{},
			wantDenied:    true,
			wantReason:    policy.DenialResourceLimitExceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := sprooziv1alpha1.AgentPolicySpec{
				AllowedCapabilities: basePolicy().AllowedCapabilities,
				EgressProfiles:      []string{},
				ResourceBounds:      sprooziv1alpha1.ResourceBounds{Max: tt.policyMax},
			}
			rt := sprooziv1alpha1.AgentRuntimeSpec{PodTemplate: corev1.
				PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: testWorkloadContainer, Resources: corev1.ResourceRequirements{Limits: tt.runtimeLimits}}}}},
			}
			denial := policy.Evaluate(p, baseTemplate(), baseRun(), rt)
			if tt.wantDenied && denial == nil {
				t.Fatal("Evaluate() = nil, want denial")
			}
			if !tt.wantDenied && denial != nil {
				t.Fatalf("Evaluate() = %+v, want nil", denial)
			}
			if tt.wantDenied && denial.Reason != tt.wantReason {
				t.Errorf("Evaluate().Reason = %q, want %q", denial.Reason, tt.wantReason)
			}
		})
	}
}

func TestEvaluateEphemeralWorkspace(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		policyEphMax  string // empty means no ephemeral-storage bound
		workspaceSize string // empty means zero (not set)
		wantDenied    bool
		wantReason    policy.DenialReason
	}{
		{
			name:          "workspace within policy bound",
			policyEphMax:  ephemeralStorageMax,
			workspaceSize: "2Gi",
			wantDenied:    false,
		},
		{
			name:          "workspace equals policy bound",
			policyEphMax:  ephemeralStorageMax,
			workspaceSize: ephemeralStorageMax,
			wantDenied:    false,
		},
		{
			name:          "workspace exceeds policy bound",
			policyEphMax:  "2Gi",
			workspaceSize: ephemeralStorageMax,
			wantDenied:    true,
			wantReason:    policy.DenialResourceLimitExceeded,
		},
		{
			name:          "no ephemeral-storage bound in policy — any workspace allowed",
			policyEphMax:  "",
			workspaceSize: "100Gi",
			wantDenied:    false,
		},
		{
			name:          "zero workspace size — always permitted",
			policyEphMax:  "1Gi",
			workspaceSize: "",
			wantDenied:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			policyMax := corev1.ResourceList{}
			if tt.policyEphMax != "" {
				policyMax[corev1.ResourceEphemeralStorage] = resource.MustParse(tt.policyEphMax)
			}
			p := sprooziv1alpha1.AgentPolicySpec{
				AllowedCapabilities: basePolicy().AllowedCapabilities,
				EgressProfiles:      []string{},
				ResourceBounds:      sprooziv1alpha1.ResourceBounds{Max: policyMax},
			}
			// Provide Resources.Limits[ephemeral-storage] equal to the policy max so
			// the resource-limits loop passes; this isolates the workspace size check.
			rtLimits := corev1.ResourceList{}
			if tt.policyEphMax != "" {
				rtLimits[corev1.ResourceEphemeralStorage] = resource.MustParse(tt.policyEphMax)
			}
			rt := sprooziv1alpha1.AgentRuntimeSpec{PodTemplate: corev1.
				PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: testWorkloadContainer, Resources: corev1.ResourceRequirements{Limits: rtLimits}}}}},
			}
			if tt.workspaceSize != "" {
				rt.EphemeralWorkspace = sprooziv1alpha1.EphemeralWorkspaceSpec{
					SizeLimit: resource.MustParse(tt.workspaceSize),
				}
			}
			denial := policy.Evaluate(p, baseTemplate(), baseRun(), rt)
			if tt.wantDenied && denial == nil {
				t.Fatal("Evaluate() = nil, want denial")
			}
			if !tt.wantDenied && denial != nil {
				t.Fatalf("Evaluate() = %+v, want nil", denial)
			}
			if tt.wantDenied && denial.Reason != tt.wantReason {
				t.Errorf("Evaluate().Reason = %q, want %q", denial.Reason, tt.wantReason)
			}
		})
	}
}

func TestEvaluateDenialMessage(t *testing.T) {
	t.Parallel()

	p := basePolicy()
	denial := policy.Evaluate(p, baseTemplate(egressGoModules), baseRun(sprooziv1alpha1.CapabilityNetworkEgress), baseRuntime())
	if denial == nil {
		t.Fatal("Evaluate() = nil, want denial")
	}
	if denial.Message == "" {
		t.Error("Denial.Message must not be empty")
	}
}
