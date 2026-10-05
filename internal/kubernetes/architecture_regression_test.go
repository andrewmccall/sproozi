package kubernetes_test

import (
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/kubernetes"
	"github.com/andrewmccall/sproozi/internal/policy"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestAdmissionBoundsTheActualPodIncludingSidecars(t *testing.T) {
	for _, limits := range [][]string{{"8"}, {"600m", "600m"}} {
		t.Run(limits[0], func(t *testing.T) {
			rt := testRuntime()
			first := rt.Spec.PodTemplate.Spec.Containers[0]
			rt.Spec.PodTemplate.Spec.Containers = nil
			for i, limit := range limits {
				c := *first.DeepCopy()
				if i > 0 {
					c.Name = "sidecar"
				}
				c.Resources.Limits[corev1.ResourceCPU] = resource.MustParse(limit)
				rt.Spec.PodTemplate.Spec.Containers = append(rt.Spec.PodTemplate.Spec.Containers, c)
			}
			pol := api.AgentPolicySpec{ResourceBounds: api.ResourceBounds{Max: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}}}
			run := testRunForSandbox("resource-regression")
			if err := rt.Spec.ValidateRuntimeTemplate(); err != nil {
				t.Fatal(err)
			}
			if policy.Evaluate(pol, api.AgentTemplateSpec{}, run.Spec, rt.Spec) == nil {
				t.Fatal("admitted containers exceeding aggregate CPU ceiling")
			}
			pod := kubernetes.BuildPodSpec(run, rt, "run-sa")
			if len(pod.Spec.Containers) != len(limits) {
				t.Fatal("provisioning discarded runtime containers")
			}
			for i, c := range pod.Spec.Containers {
				if c.Resources.Limits.Cpu().Cmp(resource.MustParse(limits[i])) != 0 {
					t.Fatal("provisioning changed admitted resource limits")
				}
			}
		})
	}
}
func TestEgressUsesOneGatewayTokenAcrossWorkloadContainers(t *testing.T) {
	rt := testRuntime()
	second := *rt.Spec.PodTemplate.Spec.Containers[0].DeepCopy()
	second.Name = "helper"
	rt.Spec.PodTemplate.Spec.Containers = append(rt.Spec.PodTemplate.Spec.Containers, second)
	rt.Spec.WorkloadContainers = append(rt.Spec.WorkloadContainers, "helper")
	run := testRunForSandbox("egress-regression")
	run.Spec.Capabilities = []api.CapabilityKind{api.CapabilityNetworkEgress}
	pod := kubernetes.BuildPodSpec(run, rt, "run-sa")
	names := map[string]bool{}
	tokens := 0
	for _, v := range pod.Spec.Volumes {
		if names[v.Name] {
			t.Fatalf("duplicate volume %s", v.Name)
		}
		names[v.Name] = true
		if v.Projected != nil {
			for _, source := range v.Projected.Sources {
				if source.ServiceAccountToken != nil {
					tokens++
					if source.ServiceAccountToken.Audience != "sproozi-gateway" {
						t.Fatal("obsolete token audience")
					}
				}
			}
		}
	}
	if tokens != 1 {
		t.Fatalf("projected tokens=%d", tokens)
	}
	for _, c := range pod.Spec.Containers {
		paths := map[string]bool{}
		for _, m := range c.VolumeMounts {
			if paths[m.MountPath] {
				t.Fatalf("duplicate mount %s", m.MountPath)
			}
			paths[m.MountPath] = true
		}
	}
}
func TestRuntimeGatewayEndpointIsUsedByPodAndContract(t *testing.T) {
	run, tmpl, rt := contractInputs()
	rt.Spec.GatewayEndpoint = "https://custom-gateway.sproozi-system.svc:8443"
	pod := kubernetes.BuildPodSpec(run, rt, "sa")
	input, err := kubernetes.BuildAgentContract(run, tmpl, rt)
	if err != nil {
		t.Fatal(err)
	}
	if input.Gateway.Endpoint != rt.Spec.GatewayEndpoint {
		t.Fatal("contract ignored gateway endpoint")
	}
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "SPROOZI_GATEWAY" {
			if env.Value != input.Gateway.Endpoint {
				t.Fatal("Pod and contract gateway endpoints differ")
			}
			return
		}
	}
	t.Fatal("Pod has no gateway endpoint")
}
