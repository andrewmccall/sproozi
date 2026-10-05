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
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	k8sresources "github.com/andrewmccall/sproozi/internal/kubernetes"
)

// ensureNamespace creates the given namespace if it does not exist.
func ensureNamespace(t *testing.T, c client.Client, ctx context.Context, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	_ = c.Create(ctx, ns) // AlreadyExists is OK
}

// TestAgentRunIdentityProvisioned verifies that when a run is admitted, the
// ServiceAccount and NetworkPolicy are created. The ServiceAccount is an
// immutable gateway identity only; no workload RoleBinding is provisioned.
func TestAgentRunIdentityProvisioned(t *testing.T) {
	c, ctx := startAgentRunController(t)
	g := gomega.NewGomegaWithT(t)

	ensureNamespace(t, c, ctx, k8sresources.AgentsNamespace)
	ensureNamespace(t, c, ctx, testSproozDemoNamespace)

	createControllerFixtures(t, c, ctx, testNamespace)

	run := validAgentRun("identity-run-1", testNamespace)
	if err := c.Create(ctx, run); err != nil {
		t.Fatalf("Create run error = %v", err)
	}

	// Wait for the run to be admitted and ServiceAccountName to be set.
	var saName string
	g.Eventually(func() string {
		var r sprooziv1alpha1.AgentRun
		if err := c.Get(ctx, client.ObjectKeyFromObject(run), &r); err != nil {
			return ""
		}
		return r.Status.Identity.ServiceAccountName
	}, 30*time.Second, 200*time.Millisecond).ShouldNot(gomega.BeEmpty(),
		"ServiceAccountName should be set in status after admission")

	// Retrieve the final SA name.
	var r sprooziv1alpha1.AgentRun
	if err := c.Get(ctx, client.ObjectKeyFromObject(run), &r); err != nil {
		t.Fatalf("Get run error = %v", err)
	}
	saName = r.Status.Identity.ServiceAccountName

	// Assert ServiceAccount exists in AgentsNamespace.
	sa := &corev1.ServiceAccount{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Name: saName, Namespace: k8sresources.AgentsNamespace}, sa)
	}, 10*time.Second, 200*time.Millisecond).Should(gomega.Succeed(),
		"ServiceAccount should exist in sproozi-agents namespace")

	// Assert NetworkPolicy exists in AgentsNamespace.
	np := &networkingv1.NetworkPolicy{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Name: saName, Namespace: k8sresources.AgentsNamespace}, np)
	}, 10*time.Second, 200*time.Millisecond).Should(gomega.Succeed(),
		"NetworkPolicy should exist in sproozi-agents namespace")
}

// TestAgentRunIdentityRevokedOnCancellation verifies that when a run is
// cancelled, the ServiceAccount and NetworkPolicy are deleted.
func TestAgentRunIdentityRevokedOnCancellation(t *testing.T) {
	c, ctx := startAgentRunController(t)
	g := gomega.NewGomegaWithT(t)

	ensureNamespace(t, c, ctx, k8sresources.AgentsNamespace)
	ensureNamespace(t, c, ctx, testSproozDemoNamespace)

	createControllerFixtures(t, c, ctx, testNamespace)

	run := validAgentRun("identity-run-2", testNamespace)
	if err := c.Create(ctx, run); err != nil {
		t.Fatalf("Create run error = %v", err)
	}

	// Wait for ServiceAccountName to be set.
	var saName string
	g.Eventually(func() string {
		var r sprooziv1alpha1.AgentRun
		if err := c.Get(ctx, client.ObjectKeyFromObject(run), &r); err != nil {
			return ""
		}
		return r.Status.Identity.ServiceAccountName
	}, 30*time.Second, 200*time.Millisecond).ShouldNot(gomega.BeEmpty())

	// Retrieve the SA name.
	var r sprooziv1alpha1.AgentRun
	if err := c.Get(ctx, client.ObjectKeyFromObject(run), &r); err != nil {
		t.Fatalf("Get run error = %v", err)
	}
	saName = r.Status.Identity.ServiceAccountName

	// Verify ServiceAccount exists before cancellation.
	sa := &corev1.ServiceAccount{}
	if err := c.Get(ctx, client.ObjectKey{Name: saName, Namespace: k8sresources.AgentsNamespace}, sa); err != nil {
		t.Fatalf("ServiceAccount should exist before cancellation: %v", err)
	}

	// Cancel the run.
	if err := c.Get(ctx, client.ObjectKeyFromObject(run), &r); err != nil {
		t.Fatalf("Get run error = %v", err)
	}
	r.Spec.Cancel = true
	if err := c.Update(ctx, &r); err != nil {
		t.Fatalf("Update run error = %v", err)
	}

	// Wait for ServiceAccount to be deleted.
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Name: saName, Namespace: k8sresources.AgentsNamespace}, sa)
	}, 30*time.Second, 200*time.Millisecond).ShouldNot(gomega.Succeed(),
		"ServiceAccount should be deleted after run cancellation")

	// Assert NetworkPolicy is deleted from AgentsNamespace.
	np := &networkingv1.NetworkPolicy{}
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Name: saName, Namespace: k8sresources.AgentsNamespace}, np)
	}, 10*time.Second, 200*time.Millisecond).ShouldNot(gomega.Succeed(),
		"NetworkPolicy should be deleted from sproozi-agents after run cancellation")
}

// TestAgentRunDoesNotProvisionWorkloadRBAC verifies that Kubernetes
// authorization is centralized in the shared gateway rather than delegated to
// each workload ServiceAccount.
func TestAgentRunDoesNotProvisionWorkloadRBAC(t *testing.T) {
	c, ctx := startAgentRunController(t)
	g := gomega.NewGomegaWithT(t)

	ensureNamespace(t, c, ctx, k8sresources.AgentsNamespace)
	ensureNamespace(t, c, ctx, testSproozDemoNamespace)

	createControllerFixtures(t, c, ctx, testNamespace)

	run := validAgentRun("identity-run-3", testNamespace)
	if err := c.Create(ctx, run); err != nil {
		t.Fatalf("Create run error = %v", err)
	}

	// Wait for ServiceAccountName to be set.
	var saName string
	g.Eventually(func() string {
		var r sprooziv1alpha1.AgentRun
		if err := c.Get(ctx, client.ObjectKeyFromObject(run), &r); err != nil {
			return ""
		}
		return r.Status.Identity.ServiceAccountName
	}, 30*time.Second, 200*time.Millisecond).ShouldNot(gomega.BeEmpty())

	// Retrieve the SA name.
	var r sprooziv1alpha1.AgentRun
	if err := c.Get(ctx, client.ObjectKeyFromObject(run), &r); err != nil {
		t.Fatalf("Get run error = %v", err)
	}
	saName = r.Status.Identity.ServiceAccountName

	role := &rbacv1.Role{}
	if err := c.Get(ctx, client.ObjectKey{Name: saName, Namespace: testSproozDemoNamespace}, role); err == nil {
		t.Fatal("workload Role must not be provisioned")
	}
	binding := &rbacv1.RoleBinding{}
	if err := c.Get(ctx, client.ObjectKey{Name: saName, Namespace: testSproozDemoNamespace}, binding); err == nil {
		t.Fatal("workload RoleBinding must not be provisioned")
	}
}
