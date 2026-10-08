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
	"fmt"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

var digestImagePattern = regexp.MustCompile(`^[A-Za-z0-9./_:-]+@sha256:[a-f0-9]{64}$`)

// ValidateRuntimeTemplate checks the administrator-owned template at the
// runtime admission seam. It deliberately fails closed for Pod features that
// could escape the sandbox or silently weaken its security boundary.
func (s *AgentRuntimeSpec) ValidateRuntimeTemplate() error {
	if !slices.Contains([]string{"", "codex", "claude-code", "opencode"}, s.ClientConfig.Harness) {
		return fmt.Errorf("unsupported harness %q", s.ClientConfig.Harness)
	}
	if len(s.PodTemplate.Spec.Containers) == 0 {
		return fmt.Errorf("podTemplate must contain a container")
	}
	// Additional container classes are not part of the approved runtime surface.
	// Reject them rather than copying containers which have not passed the same
	// image, resource, and security-context validation as normal containers.
	if len(s.PodTemplate.Spec.InitContainers) > 0 {
		return fmt.Errorf("podTemplate initContainers are forbidden")
	}
	if len(s.PodTemplate.Spec.EphemeralContainers) > 0 {
		return fmt.Errorf("podTemplate ephemeralContainers are forbidden")
	}
	if err := validateWorkloadNames(s.WorkloadContainers); err != nil {
		return err
	}
	seen := map[string]bool{}
	reserved := map[string]bool{"contract": true, "client-trust": true, "workspace": true, "tmp": true, "agent-home": true, "kubernetes-token": true, "gateway-token": true, "egress-token": true}
	for _, c := range s.PodTemplate.Spec.Containers {
		if c.Name == "" || seen[c.Name] {
			return fmt.Errorf("podTemplate container names must be unique")
		}
		seen[c.Name] = true
		if !digestImagePattern.MatchString(c.Image) {
			return fmt.Errorf("container %q image must be digest pinned", c.Name)
		}
		if c.Resources.Limits == nil || c.Resources.Limits.Cpu().IsZero() || c.Resources.Limits.Memory().IsZero() {
			return fmt.Errorf("container %q must declare CPU and memory limits", c.Name)
		}
		if unsafeContainerSecurity(c.SecurityContext) {
			return fmt.Errorf("container %q has unsafe security settings", c.Name)
		}
	}
	for _, name := range s.WorkloadContainers {
		if !seen[name] {
			return fmt.Errorf("workload container %q is not in podTemplate", name)
		}
	}
	p := s.PodTemplate.Spec
	if missingNonRootPodUser(p.SecurityContext) {
		return fmt.Errorf("podTemplate must set a non-root Pod user")
	}
	if forbiddenPodSecurity(p) {
		return fmt.Errorf("podTemplate uses a forbidden Pod security setting")
	}
	for _, v := range p.Volumes {
		if reserved[v.Name] {
			return fmt.Errorf("podTemplate volume %q is reserved", v.Name)
		}
		if v.HostPath != nil || v.PersistentVolumeClaim != nil || v.CSI != nil || v.Ephemeral != nil {
			return fmt.Errorf("volume %q uses an unapproved source", v.Name)
		}
		if v.EmptyDir != nil && v.EmptyDir.SizeLimit == nil {
			return fmt.Errorf("writable volume %q must have a size limit", v.Name)
		}
	}
	if strings.TrimSpace(string(s.PodTemplate.Spec.RestartPolicy)) != "" && s.PodTemplate.Spec.RestartPolicy != corev1.RestartPolicyNever {
		return fmt.Errorf("restartPolicy must be Never")
	}
	return nil
}

func validateWorkloadNames(names []string) error {
	if len(names) == 0 {
		return fmt.Errorf("workloadContainers must not be empty")
	}
	selected := map[string]bool{}
	for _, name := range names {
		if name == "" || selected[name] {
			return fmt.Errorf("workloadContainers must contain unique non-empty names")
		}
		selected[name] = true
	}
	return nil
}

func unsafeContainerSecurity(security *corev1.SecurityContext) bool {
	return security == nil ||
		security.Privileged != nil && *security.Privileged ||
		security.AllowPrivilegeEscalation == nil ||
		*security.AllowPrivilegeEscalation ||
		security.ReadOnlyRootFilesystem == nil ||
		!*security.ReadOnlyRootFilesystem ||
		security.RunAsNonRoot == nil ||
		!*security.RunAsNonRoot ||
		security.SeccompProfile == nil ||
		security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault ||
		security.Capabilities == nil ||
		len(security.Capabilities.Drop) == 0
}

func missingNonRootPodUser(security *corev1.PodSecurityContext) bool {
	return security == nil ||
		security.RunAsNonRoot == nil ||
		!*security.RunAsNonRoot ||
		security.RunAsUser == nil ||
		*security.RunAsUser <= 0
}

func forbiddenPodSecurity(p corev1.PodSpec) bool {
	return p.HostNetwork ||
		p.HostPID ||
		p.HostIPC ||
		(p.HostUsers != nil && !*p.HostUsers) ||
		p.ServiceAccountName != "" ||
		p.AutomountServiceAccountToken == nil ||
		*p.AutomountServiceAccountToken ||
		p.SecurityContext != nil && len(p.SecurityContext.Sysctls) > 0
}

func boolPtr(v bool) *bool { return &v }

// AgentRuntimeSpec defines the administrator-owned configuration for a Sandbox.
type AgentRuntimeSpec struct {
	// PodTemplate is the administrator-owned template copied into each Sandbox.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="has(self.spec.securityContext) && has(self.spec.securityContext.runAsNonRoot) && self.spec.securityContext.runAsNonRoot && has(self.spec.securityContext.runAsUser) && self.spec.securityContext.runAsUser > 0",message="Pod securityContext must require a non-root user"
	PodTemplate corev1.PodTemplateSpec `json:"podTemplate"`

	// WorkloadContainers names the containers which receive run-specific bindings.
	// All other containers are retained as administrator-approved sidecars.
	// +kubebuilder:validation:MinItems=1
	WorkloadContainers []string `json:"workloadContainers"`

	// ClientConfig selects administrator-owned public client configuration mounted into every Sandbox.
	ClientConfig RuntimeClientConfig `json:"clientConfig"`

	// RuntimeClassName selects the RuntimeClass for Sandbox pods.
	// +optional
	RuntimeClassName string `json:"runtimeClassName,omitempty"`

	// EphemeralWorkspace configures the disposable Sandbox workspace.
	EphemeralWorkspace EphemeralWorkspaceSpec `json:"ephemeralWorkspace"`

	// GatewayEndpoint is the one authenticated HTTPS proxy used by every capability.
	// +kubebuilder:validation:Pattern=`^https://[a-z0-9.-]+(:[0-9]+)?$`
	GatewayEndpoint string `json:"gatewayEndpoint"`
}

// RuntimeClientConfig selects non-secret, administrator-owned client material.
type RuntimeClientConfig struct {
	// Harness selects supported run-local MCP configuration rendering. Empty
	// leaves delivery to the administrator's own client launch command.
	// +optional
	// +kubebuilder:validation:Enum=codex;claude-code;opencode
	Harness string `json:"harness,omitempty"`

	// TrustBundleConfigMap identifies an immutable certificate-only ConfigMap in the
	// sproozi-agents namespace. The selected key contains public roots plus the
	// dedicated inspection CA; private keys and provider credentials are forbidden.
	TrustBundleConfigMap RuntimeConfigMapKeyReference `json:"trustBundleConfigMap"`
}

// RuntimeConfigMapKeyReference selects exactly one ConfigMap data key.
type RuntimeConfigMapKeyReference struct {
	// Name is the ConfigMap name in the sproozi-agents namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`

	// Key is the certificate bundle key.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+$`
	Key string `json:"key"`
}

// EphemeralWorkspaceSpec configures the disposable workspace volume.
type EphemeralWorkspaceSpec struct {
	// SizeLimit is the maximum capacity of the workspace volume.
	SizeLimit resource.Quantity `json:"sizeLimit"`
}

// AgentRuntimeStatus defines the observed state of AgentRuntime.
type AgentRuntimeStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the AgentRuntime resource.
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

// AgentRuntime is the Schema for the agentruntimes API
type AgentRuntime struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of AgentRuntime
	// +required
	Spec AgentRuntimeSpec `json:"spec"`

	// status defines the observed state of AgentRuntime
	// +optional
	Status AgentRuntimeStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AgentRuntimeList contains a list of AgentRuntime
type AgentRuntimeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []AgentRuntime `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &AgentRuntime{}, &AgentRuntimeList{})
		return nil
	})
}
