package envtest

import (
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

func TestAgentPolicyRejectsUnknownCapability(t *testing.T) {
	t.Parallel()

	apiClient := testClient
	ns := newNamespace(t)

	policy := validAgentPolicy(ns)
	policy.Spec.AllowedCapabilities = []sprooziv1alpha1.CapabilityKind{"unknown.capability"}
	if err := apiClient.Create(t.Context(), policy); err == nil {
		t.Fatal("Create() error = nil, want API server validation error")
	}
	policy = validAgentPolicy(ns)
	policy.Name = "mcp-policy"
	policy.Spec.AllowedCapabilities = []sprooziv1alpha1.CapabilityKind{testMCPCapability}
	arguments := []byte(`{"required":["tenant"],"properties":{"tenant":{"const":"home-ops"}}}`)
	policy.Spec.MCPServers = map[string]sprooziv1alpha1.MCPServerScope{
		"docs": {Tools: map[string]sprooziv1alpha1.MCPToolScope{
			"search": {Arguments: &runtime.RawExtension{Raw: arguments}},
		}},
	}
	policy.Spec.Budgets = map[sprooziv1alpha1.CapabilityKind]sprooziv1alpha1.EndpointBudget{
		testMCPCapability: {MaxUnits: 5},
	}
	if err := apiClient.Create(t.Context(), policy); err != nil {
		t.Fatal(err)
	}
	var stored sprooziv1alpha1.AgentPolicy
	if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(policy), &stored); err != nil {
		t.Fatal(err)
	}
	var constraint map[string]any
	if err := json.Unmarshal(stored.Spec.MCPServers["docs"].Tools["search"].Arguments.Raw, &constraint); err != nil {
		t.Fatal(err)
	}
	tenant := constraint["properties"].(map[string]any)["tenant"].(map[string]any)
	if tenant["const"] != "home-ops" {
		t.Fatalf("argument schema was pruned: %v", constraint)
	}
	for _, name := range []string{"mcp.", "mcp.Bad", "mcp.bad/name"} {
		invalid := policy.DeepCopy()
		invalid.Name = "invalid-mcp"
		invalid.ResourceVersion = ""
		invalid.UID = ""
		invalid.Spec.AllowedCapabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityKind(name)}
		if err := apiClient.Create(t.Context(), invalid); err == nil {
			t.Fatalf("invalid MCP capability accepted: %s", name)
		}
	}

}

func TestAgentPolicyRejectsUnsafeKubernetesReadResource(t *testing.T) {
	t.Parallel()

	apiClient := testClient
	ns := newNamespace(t)
	policy := validAgentPolicy(ns)
	policy.Spec.KubernetesRead.Resources = []sprooziv1alpha1.KubernetesReadResource{"secrets"}
	if err := apiClient.Create(t.Context(), policy); err == nil {
		t.Fatal("Create() error = nil, want API server validation error for Secrets")
	}
}

func TestAgentPolicyDefaultsDurations(t *testing.T) {
	t.Parallel()

	apiClient := testClient
	ns := newNamespace(t)

	policy := validAgentPolicy(ns)
	policyObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(policy)
	if err != nil {
		t.Fatalf("ToUnstructured() error = %v", err)
	}
	unstructured.RemoveNestedField(policyObject, "spec", "maxExecutionDuration")
	unstructured.RemoveNestedField(policyObject, "spec", "retentionTTL")
	unstructuredPolicy := &unstructured.Unstructured{Object: policyObject}
	unstructuredPolicy.SetAPIVersion(sprooziv1alpha1.GroupVersion.String())
	unstructuredPolicy.SetKind("AgentPolicy")
	if err := apiClient.Create(t.Context(), unstructuredPolicy); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var got sprooziv1alpha1.AgentPolicy
	if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(policy), &got); err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if got.Spec.MaxExecutionDuration.Duration != time.Hour {
		t.Errorf("maxExecutionDuration = %s, want 1h", got.Spec.MaxExecutionDuration.Duration)
	}
	if got.Spec.RetentionTTL.Duration != 30*24*time.Hour {
		t.Errorf("retentionTTL = %s, want 720h", got.Spec.RetentionTTL.Duration)
	}
}

func TestAgentPolicyRejectsExecutionDurationAboveOneHour(t *testing.T) {
	t.Parallel()

	apiClient := testClient
	ns := newNamespace(t)

	policy := validAgentPolicy(ns)
	policy.Spec.MaxExecutionDuration = metav1.Duration{Duration: time.Hour + time.Minute}
	if err := apiClient.Create(t.Context(), policy); err == nil {
		t.Fatal("Create() error = nil, want API server validation error")
	}
}

func validAgentPolicy(ns string) *sprooziv1alpha1.AgentPolicy {
	return &sprooziv1alpha1.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: testPolicyName, Namespace: ns},
		Spec: sprooziv1alpha1.AgentPolicySpec{
			AllowedCapabilities: []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityKubernetesRead},
			KubernetesRead: sprooziv1alpha1.KubernetesReadScope{
				Namespaces: []string{testSproozDemoNamespace},
				Resources:  []sprooziv1alpha1.KubernetesReadResource{testPodsResource},
			},
			GitHubPullRequest: sprooziv1alpha1.GitHubPullRequestScope{
				Repositories:        []string{"andrewmccall/home-ops"},
				AllowedBaseBranches: []string{"main"},
			},
			PackagesInstall: sprooziv1alpha1.PackagesInstallScope{
				Ecosystems:       []sprooziv1alpha1.PackageEcosystem{sprooziv1alpha1.PackageEcosystemPyPI},
				Artifacts:        []sprooziv1alpha1.PackageArtifact{},
				MaxDownloadBytes: 1,
			},
			EgressProfiles: []string{testEgressModule},
			Budgets: map[sprooziv1alpha1.CapabilityKind]sprooziv1alpha1.EndpointBudget{
				sprooziv1alpha1.CapabilityModelInference: {
					MaxUnits:      100_000,
					MaxCostMicros: 500_000,
				}},
			ResourceBounds: sprooziv1alpha1.ResourceBounds{Max: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("1"),
				corev1.ResourceMemory:           resource.MustParse("1Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
			}},
			MaxExecutionDuration: metav1.Duration{Duration: time.Hour},
			RetentionTTL:         metav1.Duration{Duration: 30 * 24 * time.Hour},
		},
	}
}

func TestAgentPolicyAllowsTokenOnlyBudgetButRejectsInvalidLimits(t *testing.T) {
	t.Parallel()
	apiClient := testClient
	ns := newNamespace(t)
	for _, tc := range []struct {
		name         string
		tokens, cost int64
		valid        bool
	}{
		{"token-only", 100_000, 0, true},
		{"negative-cost", 100_000, -1, false},
		{"zero-tokens", 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := validAgentPolicy(ns)
			policy.Name = tc.name
			policy.Spec.Budgets[sprooziv1alpha1.CapabilityModelInference] = sprooziv1alpha1.EndpointBudget{MaxUnits: tc.tokens,
				MaxCostMicros: tc.cost}
			err := apiClient.Create(t.Context(), policy)
			if (err == nil) != tc.valid {
				t.Fatalf("Create() error = %v, valid = %t", err, tc.valid)
			}
		})
	}
	t.Run("omitted-budget", func(t *testing.T) {
		policy := validAgentPolicy(ns)
		policy.Name = "omitted-budget"
		policy.Spec.Budgets = nil
		if err := apiClient.Create(t.Context(), policy); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unknown-budget-capability", func(t *testing.T) {
		policy := validAgentPolicy(ns)
		policy.Name = "unknown-budget"
		policy.Spec.Budgets = map[sprooziv1alpha1.CapabilityKind]sprooziv1alpha1.EndpointBudget{
			"model.inferance": {MaxUnits: 100},
		}
		if err := apiClient.Create(t.Context(), policy); err == nil {
			t.Fatal("accepted misspelled budget capability")
		}
	})

}
