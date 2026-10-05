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
	k8sresources "github.com/andrewmccall/sproozi/internal/kubernetes"
)

// TestAgentRunSandboxPodCreatedOnRunning verifies that when an AgentRun
// reaches Running phase, a Pod is created in sproozi-agents.
func TestAgentRunSandboxPodCreatedOnRunning(t *testing.T) {
	c, ctx := startAgentRunController(t)
	g := gomega.NewGomegaWithT(t)

	ensureNamespace(t, c, ctx, k8sresources.AgentsNamespace)
	ensureNamespace(t, c, ctx, testSproozDemoNamespace)
	createControllerFixtures(t, c, ctx, testNamespace)

	run := validAgentRun("sandbox-run-1", testNamespace)
	if err := c.Create(ctx, run); err != nil {
		t.Fatalf("Create run error = %v", err)
	}

	// Wait for the run to reach Running and have a SandboxName.
	g.Eventually(func() string {
		var r sprooziv1alpha1.AgentRun
		if err := c.Get(ctx, client.ObjectKeyFromObject(run), &r); err != nil {
			return ""
		}
		return r.Status.Identity.SandboxName
	}, 30*time.Second, 200*time.Millisecond).ShouldNot(gomega.BeEmpty(),
		"SandboxName should be set when run is Running")

	// Verify the Phase is Running.
	var r sprooziv1alpha1.AgentRun
	if err := c.Get(ctx, client.ObjectKeyFromObject(run), &r); err != nil {
		t.Fatalf("Get run error: %v", err)
	}
	if r.Status.Phase != sprooziv1alpha1.AgentRunPhaseRunning {
		t.Errorf("phase = %q, want Running", r.Status.Phase)
	}

	// Verify the Pod exists in sproozi-agents.
	sandboxName := r.Status.Identity.SandboxName
	var pod corev1.Pod
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: k8sresources.AgentsNamespace, Name: sandboxName}, &pod)
	}, 10*time.Second, 200*time.Millisecond).Should(gomega.Succeed(),
		"Sandbox Pod should exist in sproozi-agents")

	// Verify Pod security context: non-root, no privilege escalation, read-only filesystem.
	if pod.Spec.Containers[0].SecurityContext == nil {
		t.Error("Pod container SecurityContext is nil")
	} else {
		sc := pod.Spec.Containers[0].SecurityContext
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Error("AllowPrivilegeEscalation must be false")
		}
		if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
			t.Error("ReadOnlyRootFilesystem must be true")
		}
	}
	// Verify RestartPolicy.
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never", pod.Spec.RestartPolicy)
	}
	// The API defaults container fields and resource requests after creation.
	// Reconciliation must still accept its hardened, existing Pod.
	var rt sprooziv1alpha1.AgentRuntime
	if err := c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testRuntimeName}, &rt); err != nil {
		t.Fatal(err)
	}
	if _, err := k8sresources.NewPodSandboxClient(c).Ensure(ctx, &r, &rt); err != nil {
		t.Fatalf("reconcile API-defaulted sandbox: %v", err)
	}
}

// TestAgentRunSandboxPodDeletedOnCancellation verifies that cancelling a Running
// AgentRun removes the Sandbox Pod.
func TestAgentRunSandboxPodDeletedOnCancellation(t *testing.T) {
	c, ctx := startAgentRunController(t)
	g := gomega.NewGomegaWithT(t)

	ensureNamespace(t, c, ctx, k8sresources.AgentsNamespace)
	ensureNamespace(t, c, ctx, testSproozDemoNamespace)
	createControllerFixtures(t, c, ctx, testNamespace)

	run := validAgentRun("sandbox-run-2", testNamespace)
	if err := c.Create(ctx, run); err != nil {
		t.Fatalf("Create run error = %v", err)
	}

	// Wait for SandboxName to be set.
	var sandboxName string
	g.Eventually(func() string {
		var r sprooziv1alpha1.AgentRun
		if err := c.Get(ctx, client.ObjectKeyFromObject(run), &r); err != nil {
			return ""
		}
		sandboxName = r.Status.Identity.SandboxName
		return sandboxName
	}, 30*time.Second, 200*time.Millisecond).ShouldNot(gomega.BeEmpty(),
		"SandboxName must be set before cancellation test can proceed")

	// Cancel the run.
	var r sprooziv1alpha1.AgentRun
	if err := c.Get(ctx, client.ObjectKeyFromObject(run), &r); err != nil {
		t.Fatalf("Get run error: %v", err)
	}
	r.Spec.Cancel = true
	if err := c.Update(ctx, &r); err != nil {
		t.Fatalf("Cancel run: %v", err)
	}

	// Wait for the Sandbox Pod to be deleted.
	g.Eventually(func() bool {
		var pod corev1.Pod
		err := c.Get(ctx, client.ObjectKey{Namespace: k8sresources.AgentsNamespace, Name: sandboxName}, &pod)
		return err != nil // true = Pod is gone (NotFound or other error)
	}, 30*time.Second, 200*time.Millisecond).Should(gomega.BeTrue(),
		"Sandbox Pod should be deleted after run cancellation")
}
