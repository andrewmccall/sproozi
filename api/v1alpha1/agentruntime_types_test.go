package v1alpha1

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"k8s.io/utils/ptr"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	runtimeTestContainer = "agent"
)

func TestRuntimeDigestPinAllowsLocalRegistryPort(t *testing.T) {
	spec := AgentRuntimeSpec{
		PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532))},
			AutomountServiceAccountToken: boolPtr(false),
			Containers: []corev1.Container{{
				Name: runtimeTestContainer, Image: "localhost:5001/sproozi-codex@sha256:" + strings.Repeat("a", 64),
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")}},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: boolPtr(false), ReadOnlyRootFilesystem: boolPtr(true), RunAsNonRoot: boolPtr(true),
					Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
			}},
		}}, WorkloadContainers: []string{runtimeTestContainer},
	}
	if err := spec.ValidateRuntimeTemplate(); err != nil {
		t.Fatalf("digest-pinned local registry image rejected: %v", err)
	}
	for _, image := range []string{"localhost:5001/sproozi-codex:demo", "localhost:5001/sproozi-codex@sha256:short"} {
		spec.PodTemplate.Spec.Containers[0].Image = image
		if err := spec.ValidateRuntimeTemplate(); err == nil {
			t.Fatalf("invalid digest pin accepted: %s", image)
		}
	}
}

func TestValidateRuntimeTemplateRejectsUnsafeTemplate(t *testing.T) {
	admin := true
	rt := AgentRuntimeSpec{PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: runtimeTestContainer, Image: "registry.example/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")}}, SecurityContext: &corev1.SecurityContext{Privileged: &admin}}}}}, WorkloadContainers: []string{runtimeTestContainer}}
	if err := rt.ValidateRuntimeTemplate(); err == nil {
		t.Fatal("unsafe privileged template was accepted")
	}
}

func TestValidateRuntimeTemplateRejectsAdditionalContainerClasses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*corev1.PodSpec)
	}{
		{
			name: "init container",
			mutate: func(spec *corev1.PodSpec) {
				spec.InitContainers = []corev1.Container{{Name: "setup", Image: "registry.example/setup@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}
			},
		},
		{
			name: "ephemeral container",
			mutate: func(spec *corev1.PodSpec) {
				spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: "registry.example/debug@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allowPrivilegeEscalation := false
			readOnlyRootFilesystem := true
			runAsNonRoot := true
			runtimeDefault := corev1.SeccompProfileTypeRuntimeDefault
			spec := AgentRuntimeSpec{
				PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					AutomountServiceAccountToken: boolPtr(false),
					Containers: []corev1.Container{{
						Name:  runtimeTestContainer,
						Image: "registry.example/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
						Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi"),
						}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &allowPrivilegeEscalation,
							ReadOnlyRootFilesystem:   &readOnlyRootFilesystem,
							RunAsNonRoot:             &runAsNonRoot,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: runtimeDefault},
						},
					}},
				}},
				WorkloadContainers: []string{runtimeTestContainer},
			}
			tc.mutate(&spec.PodTemplate.Spec)

			if err := spec.ValidateRuntimeTemplate(); err == nil {
				t.Fatalf("ValidateRuntimeTemplate accepted an %s", tc.name)
			}
		})
	}
}

func TestAgentRuntimeJSONRoundTrip(t *testing.T) {
	t.Parallel()

	want := AgentRuntime{
		Spec: AgentRuntimeSpec{
			RuntimeClassName: "gvisor",
			PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:  runtimeTestContainer,
				Image: "ghcr.io/openai/codex@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:              resource.MustParse("1"),
						corev1.ResourceMemory:           resource.MustParse("1Gi"),
						corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
					},
				}}}, SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptr.
					To(true), RunAsUser: ptr.
					To(int64(65532)), FSGroup: ptr.To(int64(65532)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			}},
			WorkloadContainers: []string{runtimeTestContainer},
			ClientConfig: RuntimeClientConfig{
				TrustBundleConfigMap: RuntimeConfigMapKeyReference{
					Name: "sproozi-sandbox-trust",
					Key:  "ca-bundle.pem",
				},
			},

			EphemeralWorkspace: EphemeralWorkspaceSpec{
				SizeLimit: resource.MustParse("2Gi"),
			}, GatewayEndpoint: "https://sproozi-gateway.sproozi-system.svc:8443",
		},
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got AgentRuntime
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if !reflect.DeepEqual(got.Spec, want.Spec) {
		t.Fatalf("round-trip spec = %#v, want %#v", got.Spec, want.Spec)
	}
}
