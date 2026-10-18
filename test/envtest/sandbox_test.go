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
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
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
	if _, err := k8sresources.NewPodSandboxClient(c, c).Ensure(ctx, &r, &rt); err != nil {
		t.Fatalf("reconcile API-defaulted sandbox: %v", err)
	}
}
