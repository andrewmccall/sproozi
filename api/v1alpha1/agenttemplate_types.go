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

// AgentTemplateSpec defines a trusted, administrator-managed class of agent work.
type AgentTemplateSpec struct {
	// RuntimeRef identifies the one approved AgentRuntime used by this template.
	RuntimeRef AgentRuntimeReference `json:"runtimeRef"`

	// PolicyRef identifies the one live AgentPolicy evaluated for this template.
	PolicyRef AgentPolicyReference `json:"policyRef"`

	// Instructions are trusted operating instructions selected by an administrator.
	Instructions string `json:"instructions"`

	// EgressProfiles lists named egress profiles selected for this template.
	EgressProfiles []string `json:"egressProfiles"`
}

// AgentRuntimeReference identifies an AgentRuntime in the template namespace.
type AgentRuntimeReference struct {
	// Name is the referenced AgentRuntime name.
	Name string `json:"name"`
}

// AgentPolicyReference identifies an AgentPolicy in the template namespace.
type AgentPolicyReference struct {
	// Name is the referenced AgentPolicy name.
	Name string `json:"name"`
}

// AgentTemplateStatus defines the observed state of AgentTemplate.
type AgentTemplateStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the AgentTemplate resource.
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

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// AgentTemplate is the Schema for the agenttemplates API
type AgentTemplate struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of AgentTemplate
	// +required
	Spec AgentTemplateSpec `json:"spec"`

	// status defines the observed state of AgentTemplate
	// +optional
	Status AgentTemplateStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AgentTemplateList contains a list of AgentTemplate
type AgentTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []AgentTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &AgentTemplate{}, &AgentTemplateList{})
		return nil
	})
}
