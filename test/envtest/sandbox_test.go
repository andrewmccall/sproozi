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
package envtest

import (
	"context"
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	k8sresources "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
)

// TestAgentRunSandboxPodCreatedOnRunning verifies that a Running AgentRun has
// a Pod in sproozi-agents, and that reconciling the Pod after the real API
// server defaulted its fields still accepts it. Unit tests use a fake client
// that applies no defaults, and the controller only calls Ensure again when a
// status write is lost, so the Kind suite does not reliably reach this path.
func TestAgentRunSandboxPodCreatedOnRunning(t *testing.T) {
	c, ctx, ns := startAgentRunController(t)
	g := gomega.NewGomegaWithT(t)

	createControllerFixtures(t, c, ctx, ns)

	run := validAgentRun("sandbox-run-1", ns)
	if err := c.Create(ctx, run); err != nil {
		t.Fatalf("Create run error = %v", err)
	}

	// Wait for the run to reach Running and have a SandboxName.
	var r sprooziv1alpha1.AgentRun
	g.Eventually(func() string {
		if err := c.Get(ctx, client.ObjectKeyFromObject(run), &r); err != nil {
			return ""
		}
		return r.Status.Identity.SandboxName
	}, 30*time.Second, 200*time.Millisecond).ShouldNot(gomega.BeEmpty(),
		"SandboxName should be set when run is Running")
	if r.Status.Phase != sprooziv1alpha1.AgentRunPhaseRunning {
		t.Errorf("phase = %q, want Running", r.Status.Phase)
	}

	var pod corev1.Pod
	key := client.ObjectKey{Namespace: k8sresources.AgentsNamespace, Name: r.Status.Identity.SandboxName}
	g.Eventually(func() error {
		return c.Get(ctx, key, &pod)
	}, 10*time.Second, 200*time.Millisecond).Should(gomega.Succeed(),
		"Sandbox Pod should exist in sproozi-agents")

	var rt sprooziv1alpha1.AgentRuntime
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: testRuntimeName}, &rt); err != nil {
		t.Fatal(err)
	}
	sandboxes := k8sresources.NewPodSandboxClient(c, c).WithGatewayResolver(testGatewayResolver)
	if _, err := sandboxes.Ensure(ctx, &r, &rt); err != nil {
		t.Fatalf("reconcile API-defaulted sandbox: %v", err)
	}
}

// TestSandboxEnsureAcceptsAPIDefaultedProbesAndVolumes creates a sandbox from
// a runtime that leaves server-defaulted fields unset (probe thresholds, port
// protocol, field reference versions and volume modes), then ensures it again
// against the Pod the API server stored.
func TestSandboxEnsureAcceptsAPIDefaultedProbesAndVolumes(t *testing.T) {
	c, ctx, ns := startAgentRunController(t)
	createControllerFixtures(t, c, ctx, ns)

	rt := validAgentRuntime(ns)
	spec := &rt.Spec.PodTemplate.Spec
	labels := &corev1.ObjectFieldSelector{FieldPath: "metadata.labels"}
	spec.Volumes = []corev1.Volume{{Name: "info", VolumeSource: corev1.VolumeSource{
		DownwardAPI: &corev1.DownwardAPIVolumeSource{
			Items: []corev1.DownwardAPIVolumeFile{{Path: "labels", FieldRef: labels}}}}}}
	agent := &spec.Containers[0]
	agent.Ports = []corev1.ContainerPort{{ContainerPort: 8080}}
	podName := &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}
	agent.Env = []corev1.EnvVar{{Name: "POD", ValueFrom: &corev1.EnvVarSource{FieldRef: podName}}}
	agent.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(8080)}}}
	run := &sprooziv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "defaulted", Namespace: ns, UID: types.UID("defaulted-sandbox-uid")},
		Status: sprooziv1alpha1.AgentRunStatus{
			Identity: sprooziv1alpha1.AgentRunIdentity{ServiceAccountName: "defaulted"}},
	}

	sandboxes := k8sresources.NewPodSandboxClient(testClient, testClient).WithGatewayResolver(testGatewayResolver)
	if _, err := sandboxes.Ensure(ctx, run, rt); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() {
		_ = sandboxes.DeleteForRun(context.Background(), k8sresources.RunName(run.UID), string(run.UID))
	})
	if _, err := sandboxes.Ensure(ctx, run, rt); err != nil {
		t.Fatalf("ensure API-defaulted sandbox: %v", err)
	}
}
