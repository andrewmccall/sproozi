package v1alpha1

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAgentPolicyJSONRoundTrip(t *testing.T) {
	t.Parallel()

	want := AgentPolicy{
		Spec: AgentPolicySpec{
			AllowedCapabilities: []CapabilityKind{
				CapabilityKubernetesRead,
				CapabilityGitHubPullRequest,
			},
			KubernetesRead: KubernetesReadScope{
				Namespaces: []string{"sproozi-demo"},
				Resources:  []KubernetesReadResource{"pods", "pods/log", "events", "services", "deployments", "replicasets", "statefulsets"},
			},
			GitHubPullRequest: GitHubPullRequestScope{
				Repositories: []string{"andrewmccall/home-ops"},
			},
			PackagesInstall: PackagesInstallScope{
				Ecosystems: []PackageEcosystem{PackageEcosystemPyPI}, MaxDownloadBytes: 10 << 20,
				Artifacts: []PackageArtifact{{Name: "demo-tool", Version: "1.0.0", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Filename: "demo_tool-1.0.0-py3-none-any.whl"}},
			},
			EgressProfiles: []string{"go-modules", "npm"},
			Budgets: map[CapabilityKind]EndpointBudget{CapabilityModelInference: {
				MaxUnits:      100_000,
				MaxCostMicros: 500_000,
			}},
			ResourceBounds: ResourceBounds{
				Max: corev1.ResourceList{
					corev1.ResourceCPU:              resource.MustParse("1"),
					corev1.ResourceMemory:           resource.MustParse("1Gi"),
					corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
				},
			},
			MaxExecutionDuration: metav1.Duration{Duration: time.Hour},
			RetentionTTL:         metav1.Duration{Duration: 30 * 24 * time.Hour},
		},
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got AgentPolicy
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if !reflect.DeepEqual(got.Spec, want.Spec) {
		t.Fatalf("round-trip spec = %#v, want %#v", got.Spec, want.Spec)
	}
}
