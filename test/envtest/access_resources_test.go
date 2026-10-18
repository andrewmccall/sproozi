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
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

// ensureNamespace creates the given namespace if it does not exist.
func ensureNamespace(t *testing.T, c client.Client, ctx context.Context, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	_ = c.Create(ctx, ns) // AlreadyExists is OK
}

// TestAgentRunDoesNotProvisionWorkloadRBAC verifies that Kubernetes
// authorization is centralized in the shared gateway rather than delegated to
// each workload ServiceAccount.
func TestAgentRunDoesNotProvisionWorkloadRBAC(t *testing.T) {
	c, ctx, ns := startAgentRunController(t)
	g := gomega.NewGomegaWithT(t)

	createControllerFixtures(t, c, ctx, ns)

	run := validAgentRun("identity-run-3", ns)
	if err := c.Create(ctx, run); err != nil {
		t.Fatalf("Create run error = %v", err)
	}

	// Wait for the ServiceAccount name, which admission sets with identity.
	var saName string
	g.Eventually(func() string {
		var r sprooziv1alpha1.AgentRun
		if err := c.Get(ctx, client.ObjectKeyFromObject(run), &r); err != nil {
			return ""
		}
		saName = r.Status.Identity.ServiceAccountName
		return saName
	}, 30*time.Second, 200*time.Millisecond).ShouldNot(gomega.BeEmpty())

	role := &rbacv1.Role{}
	if err := c.Get(ctx, client.ObjectKey{Name: saName, Namespace: testSproozDemoNamespace}, role); err == nil {
		t.Fatal("workload Role must not be provisioned")
	}
	binding := &rbacv1.RoleBinding{}
	if err := c.Get(ctx, client.ObjectKey{Name: saName, Namespace: testSproozDemoNamespace}, binding); err == nil {
		t.Fatal("workload RoleBinding must not be provisioned")
	}
}
