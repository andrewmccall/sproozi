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
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=16384
	Instructions string `json:"instructions"`

	// EgressProfiles lists named egress profiles selected for this template.
	// +kubebuilder:validation:MaxItems=32
	// +listType=set
	EgressProfiles []string `json:"egressProfiles"`
}

// AgentRuntimeReference identifies an AgentRuntime in the template namespace.
type AgentRuntimeReference struct {
	// Name is the referenced AgentRuntime name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// AgentPolicyReference identifies an AgentPolicy in the template namespace.
type AgentPolicyReference struct {
	// Name is the referenced AgentPolicy name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// +kubebuilder:object:root=true

// AgentTemplate is the Schema for the agenttemplates API
type AgentTemplate struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of AgentTemplate
	// +required
	Spec AgentTemplateSpec `json:"spec"`
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
