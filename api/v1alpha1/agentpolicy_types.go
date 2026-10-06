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
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
)

// AgentPolicySpec defines the live authorization policy for AgentRuns.
type AgentPolicySpec struct {
	// AllowedCapabilities lists the capabilities that a run may request.
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:MinItems=1
	AllowedCapabilities []CapabilityKind `json:"allowedCapabilities"`

	// MCPServers constrains named MCP capabilities. Registration URLs and
	// provider credentials belong exclusively to trusted gateway configuration.
	// +optional
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'))",message="MCP server names must be DNS labels"
	MCPServers map[string]MCPServerScope `json:"mcpServers,omitempty"`

	// KubernetesRead constrains Kubernetes read operations.
	KubernetesRead KubernetesReadScope `json:"kubernetesRead"`

	// GitHubPullRequest constrains GitHub pull request operations.
	GitHubPullRequest GitHubPullRequestScope `json:"githubPullRequest"`

	// PackagesInstall constrains package artifacts that may be downloaded for a run.
	// +optional
	PackagesInstall PackagesInstallScope `json:"packagesInstall,omitempty"`

	// EgressProfiles lists the named egress profiles available to templates.
	// +kubebuilder:validation:MaxItems=32
	EgressProfiles []string `json:"egressProfiles"`

	// Budgets optionally bound consumption per capability and AgentRun.
	// Omitted capabilities have no consumption ceiling.
	// +optional
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^(kubernetes[.]read|github[.]pull_request|model[.]inference|network[.]egress|packages[.]install|mcp[.][a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)$'))",message="budget keys must be native or named MCP capabilities"
	Budgets map[CapabilityKind]EndpointBudget `json:"budgets,omitempty"`

	// ResourceBounds caps the resources selected by an AgentRuntime.
	ResourceBounds ResourceBounds `json:"resourceBounds"`

	// MaxExecutionDuration is the longest an AgentRun may execute.
	// +optional
	// +kubebuilder:default="1h"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('1h')",message="must not exceed one hour"
	MaxExecutionDuration metav1.Duration `json:"maxExecutionDuration,omitempty"`

	// RetentionTTL is how long terminal AgentRuns are retained.
	// +optional
	// +kubebuilder:default="720h"
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="must be greater than zero"
	RetentionTTL metav1.Duration `json:"retentionTTL,omitempty"`
}

// CapabilityKind identifies an operation that may be granted to an AgentRun.
// +kubebuilder:validation:MaxLength=67
// +kubebuilder:validation:Pattern=`^(kubernetes[.]read|github[.]pull_request|model[.]inference|network[.]egress|packages[.]install|mcp[.][a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)$`
type CapabilityKind string

// MCPServerName identifies a configured MCP grant without admitting arbitrary
// capability strings or treating tool installation as authority.
func (c CapabilityKind) MCPServerName() (string, bool) {
	if !strings.HasPrefix(string(c), "mcp.") {
		return "", false
	}
	name := strings.TrimPrefix(string(c), "mcp.")
	return name, len(validation.IsDNS1123Label(name)) == 0
}

// MCPServerScope grants only explicitly named tools of a registered server.
type MCPServerScope struct {
	// Tools is a default-deny map, not a copy of the server's discovered catalogue.
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=128
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^[A-Za-z0-9_.-]{1,128}$'))",message="invalid MCP tool name"
	Tools map[string]MCPToolScope `json:"tools"`
}

// MCPToolScope adds argument predicates to the discovered input schema.
type MCPToolScope struct {
	// Arguments is an optional JSON Schema restriction. It cannot replace the
	// upstream input schema or establish the tool's service semantics.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=object
	Arguments *runtime.RawExtension `json:"arguments,omitempty"`
}

const (
	CapabilityKubernetesRead    CapabilityKind = "kubernetes.read"
	CapabilityGitHubPullRequest CapabilityKind = "github.pull_request"
	CapabilityModelInference    CapabilityKind = "model.inference"
	CapabilityNetworkEgress     CapabilityKind = "network.egress"
	CapabilityPackagesInstall   CapabilityKind = "packages.install"
)

// KubernetesReadScope limits Kubernetes namespaces and resources.
type KubernetesReadScope struct {
	// Namespaces lists target namespaces the AgentRun may read.
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:MinItems=1
	Namespaces []string `json:"namespaces"`

	// Resources lists Kubernetes resources and subresources the AgentRun may read.
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:MinItems=1
	Resources []KubernetesReadResource `json:"resources"`
}

// KubernetesReadResource is one safe resource/subresource that an AgentRun may inspect.
// +kubebuilder:validation:Enum=pods;pods/log;events;services;deployments;replicasets;statefulsets
type KubernetesReadResource string

const (
	KubernetesReadPods         KubernetesReadResource = "pods"
	KubernetesReadPodLogs      KubernetesReadResource = "pods/log"
	KubernetesReadEvents       KubernetesReadResource = "events"
	KubernetesReadServices     KubernetesReadResource = "services"
	KubernetesReadDeployments  KubernetesReadResource = "deployments"
	KubernetesReadReplicaSets  KubernetesReadResource = "replicasets"
	KubernetesReadStatefulSets KubernetesReadResource = "statefulsets"
)

// GitHubPullRequestScope limits repositories where an AgentRun may open pull requests.
type GitHubPullRequestScope struct {
	// Repositories lists repositories in owner/name form.
	// +kubebuilder:validation:MaxItems=64
	Repositories []string `json:"repositories"`

	// AllowedBaseBranches lists the only target branches for created pull requests.
	// +kubebuilder:validation:MaxItems=32
	AllowedBaseBranches []string `json:"allowedBaseBranches"`
}

// PackageEcosystem identifies a supported package registry protocol.
// +kubebuilder:validation:Enum=pypi
type PackageEcosystem string

const PackageEcosystemPyPI PackageEcosystem = "pypi"

// PackagesInstallScope is an explicit allow-list for verified package artifacts.
// An empty scope grants no package-install authority, even when the capability is
// listed in AllowedCapabilities.
type PackagesInstallScope struct {
	// Ecosystems lists the supported registries for this scope.
	// +kubebuilder:validation:MaxItems=8
	Ecosystems []PackageEcosystem `json:"ecosystems"`

	// Artifacts is the complete set of wheels and locked dependencies allowed.
	// +kubebuilder:validation:MaxItems=128
	Artifacts []PackageArtifact `json:"artifacts"`

	// AllowSourceBuilds must remain false for the wheel-only implementation.
	// +optional
	AllowSourceBuilds bool `json:"allowSourceBuilds,omitempty"`

	// MaxDownloadBytes caps each artifact response, including metadata responses.
	// +kubebuilder:validation:Minimum=1
	MaxDownloadBytes int64 `json:"maxDownloadBytes"`
}

// PackageArtifact is one exact package identity and SHA-256 digest.
type PackageArtifact struct {
	// Name is the normalized package name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[a-z0-9]+([._-][a-z0-9]+)*$`
	Name string `json:"name"`
	// Version is an exact PEP 440 version string.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Version string `json:"version"`
	// SHA256 is the lowercase hexadecimal digest of the wheel bytes.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	SHA256 string `json:"sha256"`
	// Filename is the exact wheel filename advertised by registry metadata.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Filename string `json:"filename"`
	// Dependencies contains the complete locked dependency identities.
	// +kubebuilder:validation:MaxItems=64
	Dependencies []PackageDependency `json:"dependencies,omitempty"`
}

// PackageDependency identifies a dependency that must also be present in Artifacts.
type PackageDependency struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// EndpointBudget bounds one capability's consumption for an AgentRun.
// Models consume tokens; other endpoints consume upstream requests.
// +kubebuilder:validation:XValidation:rule="self.maxUnits > 0 || self.maxCostMicros > 0",message="at least one budget limit must be positive"
type EndpointBudget struct {
	// MaxUnits is the consumption ceiling; zero leaves units uncapped.
	// +kubebuilder:validation:Minimum=0
	MaxUnits int64 `json:"maxUnits"`
	// MaxCostMicros is the ceiling in millionths of a US dollar; zero is uncapped.
	// Only endpoints with trusted pricing can consume a monetary budget.
	// +kubebuilder:validation:Minimum=0
	MaxCostMicros int64 `json:"maxCostMicros"`
}

// ResourceBounds caps resources used by an AgentRuntime.
type ResourceBounds struct {
	// Max is the maximum permitted resource quantity for each resource name.
	Max corev1.ResourceList `json:"max"`
}

// AgentPolicyStatus defines the observed state of AgentPolicy.
type AgentPolicyStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the AgentPolicy resource.
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

// AgentPolicy is the Schema for the agentpolicies API
type AgentPolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of AgentPolicy
	// +required
	Spec AgentPolicySpec `json:"spec"`

	// status defines the observed state of AgentPolicy
	// +optional
	Status AgentPolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AgentPolicyList contains a list of AgentPolicy
type AgentPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []AgentPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &AgentPolicy{}, &AgentPolicyList{})
		return nil
	})
}
