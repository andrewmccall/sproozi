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
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/andrewmccall/sproozi/internal/nativeconfig"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

var digestImagePattern = regexp.MustCompile(`^[A-Za-z0-9./_:-]+@sha256:[a-f0-9]{64}$`)

const runtimeDropAll corev1.Capability = "ALL"

// ValidateRuntimeTemplate checks the administrator-owned template at the
// runtime admission seam. It deliberately fails closed for Pod features that
// could escape the sandbox or silently weaken its security boundary.
func (s *AgentRuntimeSpec) ValidateRuntimeTemplate() error {
	if err := s.ClientConfig.validateNativeSession(); err != nil {
		return err
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
		if len(c.EnvFrom) != 0 {
			return fmt.Errorf("container %q envFrom is forbidden; use explicit non-secret configuration", c.Name)
		}
		for _, env := range c.Env {
			if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
				return fmt.Errorf("container %q Secret environment references are forbidden", c.Name)
			}
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
		if err := validateRuntimeVolume(v); err != nil {
			return err
		}
	}
	if strings.TrimSpace(string(s.PodTemplate.Spec.RestartPolicy)) != "" && s.PodTemplate.Spec.RestartPolicy != corev1.RestartPolicyNever {
		return fmt.Errorf("restartPolicy must be Never")
	}
	return nil
}

func validateRuntimeVolume(v corev1.Volume) error {
	// Template volumes carry bounded scratch space or public configuration.
	// Credentials and projected tokens are injected only by the controller.
	approved := corev1.VolumeSource{EmptyDir: v.EmptyDir, ConfigMap: v.ConfigMap, DownwardAPI: v.DownwardAPI}
	sources := 0
	if v.EmptyDir != nil {
		sources++
	}
	if v.ConfigMap != nil {
		sources++
	}
	if v.DownwardAPI != nil {
		sources++
	}
	if sources != 1 || !reflect.DeepEqual(v.VolumeSource, approved) {
		return fmt.Errorf("volume %q uses an unapproved source", v.Name)
	}
	if v.EmptyDir != nil && (v.EmptyDir.SizeLimit == nil || v.EmptyDir.SizeLimit.Sign() <= 0) {
		return fmt.Errorf("writable volume %q must have a positive size limit", v.Name)
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
		!slices.Contains(security.Capabilities.Drop, runtimeDropAll) ||
		len(security.Capabilities.Add) != 0
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
		(p.ShareProcessNamespace != nil && *p.ShareProcessNamespace) ||
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
	// +kubebuilder:validation:Enum=codex;claude-code;opencode;hermes;copilot
	Harness string `json:"harness,omitempty"`

	// NativeSession supplies administrator-owned launch settings for managed
	// native sessions. Portable checkpoints never supply these options. Provider
	// credentials belong at the gateway, outside this public runtime resource.
	// +optional
	NativeSession *NativeSessionConfig `json:"nativeSession,omitempty"`

	// TrustBundleConfigMap identifies an immutable certificate-only ConfigMap in the
	// sproozi-agents namespace. The selected key contains public roots plus the
	// dedicated inspection CA; private keys and provider credentials are forbidden.
	TrustBundleConfigMap RuntimeConfigMapKeyReference `json:"trustBundleConfigMap"`
}

// NativeSessionConfig configures an approved native terminal launch.
type NativeSessionConfig struct {
	// Model selects the native model; empty uses the stock client's default.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	Model string `json:"model,omitempty"`

	// Options are stock Codex key=TOML-value settings passed at launch. The
	// initial instructions and update gate are separately owned by Sproozi.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=4096
	Options []string `json:"options,omitempty"`
}

func (c RuntimeClientConfig) validateNativeSession() error {
	if c.NativeSession == nil {
		return nil
	}
	native := c.NativeSession
	// nativeconfig accepts exactly the managed harnesses; a guard test in
	// internal/harness compares it with the registry.
	if err := nativeconfig.ValidateHarness(c.Harness, native.Model, native.Options); err != nil {
		return fmt.Errorf("native session options: %w", err)
	}
	return nil
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

// +kubebuilder:object:root=true

// AgentRuntime is the Schema for the agentruntimes API
type AgentRuntime struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of AgentRuntime
	// +required
	Spec AgentRuntimeSpec `json:"spec"`
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
