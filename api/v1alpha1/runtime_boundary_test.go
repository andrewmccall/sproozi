package v1alpha1

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

const fixtureSecretName = "credentials"

func approvedRuntimeTemplate() AgentRuntimeSpec {
	container := corev1.Container{
		Name: "agent", Image: "registry.example/agent@sha256:" + strings.Repeat("a", 64),
		Resources:       corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")}},
		SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), RunAsNonRoot: ptr.To(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{runtimeDropAll}}},
	}
	helper := *container.DeepCopy()
	helper.Name = "helper"
	return AgentRuntimeSpec{WorkloadContainers: []string{"agent"}, PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{AutomountServiceAccountToken: ptr.To(false), SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532))}, Containers: []corev1.Container{container, helper}}}}
}

func TestRuntimeContainerBoundaryAppliesToWorkloadAndHelper(t *testing.T) {
	for _, index := range []int{0, 1} {
		for _, tc := range []struct {
			name   string
			mutate func(*corev1.Container)
		}{
			{"partial capability drop", func(c *corev1.Container) { c.SecurityContext.Capabilities.Drop = []corev1.Capability{"NET_RAW"} }},
			{"added capability", func(c *corev1.Container) {
				c.SecurityContext.Capabilities.Add = []corev1.Capability{"NET_BIND_SERVICE"}
			}},
			{"Secret reference", func(c *corev1.Container) {
				c.Env = []corev1.EnvVar{{Name: "KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: fixtureSecretName}, Key: "key"}}}}
			}},
			{"envFrom", func(c *corev1.Container) {
				c.EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "config"}}}}
			}},
		} {
			t.Run([]string{"agent/", "helper/"}[index]+tc.name, func(t *testing.T) {
				spec := approvedRuntimeTemplate()
				if err := spec.ValidateRuntimeTemplate(); err != nil {
					t.Fatal(err)
				}
				tc.mutate(&spec.PodTemplate.Spec.Containers[index])
				if err := spec.ValidateRuntimeTemplate(); err == nil {
					t.Fatal("unsupported runtime admitted")
				}
			})
		}
	}
}

func TestRuntimeVolumeAndProcessBoundary(t *testing.T) {
	size := resource.MustParse("64Mi")
	zero := resource.MustParse("0")
	for _, tc := range []struct {
		name    string
		source  corev1.VolumeSource
		allowed bool
	}{
		{"bounded scratch", corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &size}}, true},
		{"public config", corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "config"}}}, true},
		{"downward metadata", corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{}}, true},
		{"unbounded scratch", corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}, false},
		{"zero scratch", corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &zero}}, false},
		{"Secret", corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: fixtureSecretName}}, false},
		{"projection", corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{}}, false},
		{"network storage", corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{Server: "storage", Path: "/data"}}, false},
		{"mixed sources", corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &size}, Secret: &corev1.SecretVolumeSource{SecretName: fixtureSecretName}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := approvedRuntimeTemplate()
			spec.PodTemplate.Spec.Volumes = []corev1.Volume{{Name: "extra", VolumeSource: tc.source}}
			if err := spec.ValidateRuntimeTemplate(); (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v error=%v", tc.allowed, err)
			}
		})
	}
	spec := approvedRuntimeTemplate()
	spec.PodTemplate.Spec.ShareProcessNamespace = ptr.To(false)
	if err := spec.ValidateRuntimeTemplate(); err != nil {
		t.Fatal(err)
	}
	spec.PodTemplate.Spec.ShareProcessNamespace = ptr.To(true)
	if err := spec.ValidateRuntimeTemplate(); err == nil {
		t.Fatal("shared process namespace admitted")
	}
}

// The control connection is delivered to every managed execution; no policy,
// registry or native launch may grant or register a server with its name.
func TestControlMCPServerNameIsNotGrantable(t *testing.T) {
	if _, valid := CapabilityKind("mcp." + ControlMCPServer).MCPServerName(); valid {
		t.Fatal("mcp.sproozi accepted as a grantable MCP server")
	}
	if name, valid := CapabilityKind("mcp.sproozi-docs").MCPServerName(); !valid || name != "sproozi-docs" {
		t.Fatalf("ordinary server name rejected: %q %v", name, valid)
	}
}
