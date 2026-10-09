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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// AgentRunSpec defines one bounded request to execute an AgentTemplate.
//
// All fields except `cancel` are immutable after creation — PRD §5 and §10
// prohibit post-creation mutation of templateRef, capabilities, task, and
// eventContext.
//
// +kubebuilder:validation:XValidation:rule="self.templateRef == oldSelf.templateRef",message="spec.templateRef is immutable"
// +kubebuilder:validation:XValidation:rule="self.task == oldSelf.task",message="spec.task is immutable"
// +kubebuilder:validation:XValidation:rule="self.capabilities == oldSelf.capabilities",message="spec.capabilities is immutable"
// +kubebuilder:validation:XValidation:rule="has(oldSelf.eventContext) == has(self.eventContext) && (!has(self.eventContext) || self.eventContext == oldSelf.eventContext)",message="spec.eventContext is immutable"
type AgentRunSpec struct {
	// TemplateRef identifies the administrator-managed template for this run.
	TemplateRef AgentTemplateReference `json:"templateRef"`

	// Task is untrusted free-form work requested from the agent.
	Task string `json:"task"`

	// EventContext is immutable untrusted evidence supplied by the caller or webhook.
	EventContext map[string]string `json:"eventContext,omitempty"`

	// Capabilities is the requested subset of capabilities permitted by the policy.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	Capabilities []CapabilityKind `json:"capabilities"`

	// Cancel requests cancellation of an active run.
	// +optional
	Cancel bool `json:"cancel,omitempty"`
}

// AgentTemplateReference identifies an AgentTemplate in the AgentRun namespace.
type AgentTemplateReference struct {
	// Name is the referenced AgentTemplate name.
	Name string `json:"name"`
}

// AgentRunPhase identifies the current lifecycle state of an AgentRun.
// +kubebuilder:validation:Enum=Queued;Admitted;Running;Succeeded;Failed;TimedOut;Cancelled
type AgentRunPhase string

const (
	AgentRunPhaseQueued    AgentRunPhase = "Queued"
	AgentRunPhaseAdmitted  AgentRunPhase = "Admitted"
	AgentRunPhaseRunning   AgentRunPhase = "Running"
	AgentRunPhaseSucceeded AgentRunPhase = "Succeeded"
	AgentRunPhaseFailed    AgentRunPhase = "Failed"
	AgentRunPhaseTimedOut  AgentRunPhase = "TimedOut"
	AgentRunPhaseCancelled AgentRunPhase = "Cancelled"
)

// AgentRunStatus defines the observed state of AgentRun.
type AgentRunStatus struct {
	// Phase is the current lifecycle state of the AgentRun.
	// +optional
	Phase AgentRunPhase `json:"phase,omitempty"`

	// Identity identifies the resources created for this AgentRun.
	// +optional
	Identity AgentRunIdentity `json:"identity,omitempty"`

	// StartedAt records when the run was admitted to execution.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt records when the run entered a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// Summary is an audit-safe brief summary of the completed run (max 500 chars).
	// +optional
	Summary string `json:"summary,omitempty"`

	// Result is optional bounded untrusted final output captured before sandbox cleanup.
	// Its presence does not determine execution success. Absence means no valid
	// result was published, including cancellation and deadline termination.
	// Readers of AgentRun status can read this output until the run retention TTL.
	// +optional
	Result *AgentRunResult `json:"result,omitempty"`

	// conditions represent the current state of the AgentRun resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// AgentRunResult holds workload-controlled final text, independently of lifecycle status.
type AgentRunResult struct {
	// Text is UTF-8 final output. An available empty answer is valid.
	// +kubebuilder:validation:MaxLength=3584
	Text string `json:"text"`

	// Truncated identifies output shortened to fit the bounded publication envelope.
	Truncated bool `json:"truncated"`
}

// AgentRunIdentity identifies disposable resources assigned to an AgentRun.
type AgentRunIdentity struct {
	// ServiceAccountName is the per-run ServiceAccount name.
	ServiceAccountName string `json:"serviceAccountName"`

	// SandboxName is the per-run Sandbox name.
	SandboxName string `json:"sandboxName"`

	// GrantedNamespaces is retained for status compatibility with older runs.
	// New runs do not provision workload RBAC; Kubernetes requests are authorized
	// by the shared gateway against the immutable run identity and live policy.
	// +optional
	GrantedNamespaces []string `json:"grantedNamespaces,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// AgentRun is the Schema for the agentruns API
type AgentRun struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of AgentRun
	// +required
	Spec AgentRunSpec `json:"spec"`

	// status defines the observed state of AgentRun
	// +optional
	Status AgentRunStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AgentRunList contains a list of AgentRun
type AgentRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []AgentRun `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &AgentRun{}, &AgentRunList{})
		return nil
	})
}
