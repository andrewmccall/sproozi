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
package kubernetes_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/kubernetes"
)

// testDefaultNS is used for AgentRun and AgentPolicy object namespace in unit tests.
const testDefaultNS = "default"

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme error: %v", err)
	}
	if err := sprooziv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme sproozi error: %v", err)
	}
	return s
}

func testRun(uid string) *sprooziv1alpha1.AgentRun {
	return &sprooziv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-run",
			Namespace: testDefaultNS,
			UID:       types.UID(uid),
		},
	}
}

func TestEnsureServiceAccountCreates(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	run := testRun("aaaaaaaa-0000-0000-0000-000000000001")

	saName, err := kubernetes.EnsureServiceAccount(context.Background(), c, run)
	if err != nil {
		t.Fatalf("EnsureServiceAccount error: %v", err)
	}
	if saName == "" {
		t.Fatal("EnsureServiceAccount returned empty name")
	}

	// SA must exist in sproozi-agents namespace
	var sa corev1.ServiceAccount
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: kubernetes.AgentsNamespace, Name: saName}, &sa); err != nil {
		t.Fatalf("ServiceAccount not found: %v", err)
	}
	// Must carry the run label
	if sa.Labels[kubernetes.RunLabel] != run.Name {
		t.Errorf("SA missing run label: %v", sa.Labels)
	}
	if sa.Labels[kubernetes.RunNamespaceLabel] != run.Namespace || sa.Labels[kubernetes.RunUIDLabel] != string(run.UID) {
		t.Errorf("SA missing immutable run identity labels: %v", sa.Labels)
	}
	// Must carry the managed-by label
	if sa.Labels[kubernetes.ManagedByLabel] != kubernetes.ManagedByValue {
		t.Errorf("SA missing managed-by label: %v", sa.Labels)
	}
}

func TestEnsureServiceAccountIdempotent(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	run := testRun("aaaaaaaa-0000-0000-0000-000000000002")

	name1, err1 := kubernetes.EnsureServiceAccount(context.Background(), c, run)
	name2, err2 := kubernetes.EnsureServiceAccount(context.Background(), c, run)
	if err1 != nil || err2 != nil {
		t.Fatalf("EnsureServiceAccount errors: %v, %v", err1, err2)
	}
	if name1 != name2 {
		t.Errorf("EnsureServiceAccount returned different names: %q vs %q", name1, name2)
	}
}

func TestEnsureServiceAccountRejectsConflictingExistingIdentity(t *testing.T) {
	run := testRun("aaaaaaaa-0000-0000-0000-000000000099")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(&corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: kubernetes.RunName(run.UID), Namespace: kubernetes.AgentsNamespace},
	}).Build()
	if _, err := kubernetes.EnsureServiceAccount(context.Background(), c, run); err == nil {
		t.Fatal("expected conflicting ServiceAccount to be rejected")
	}
}

func TestRevokeServiceAccountDeletes(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	run := testRun("aaaaaaaa-0000-0000-0000-000000000003")

	saName, err := kubernetes.EnsureServiceAccount(context.Background(), c, run)
	if err != nil {
		t.Fatalf("EnsureServiceAccount error: %v", err)
	}

	if err := kubernetes.RevokeServiceAccount(context.Background(), c, saName); err != nil {
		t.Fatalf("RevokeServiceAccount error: %v", err)
	}

	var sa corev1.ServiceAccount
	err = c.Get(context.Background(), client.ObjectKey{Namespace: kubernetes.AgentsNamespace, Name: saName}, &sa)
	if err == nil {
		t.Error("ServiceAccount still exists after revoke")
	}
}

func TestRevokeServiceAccountNotFoundIsOK(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	if err := kubernetes.RevokeServiceAccount(context.Background(), c, "sproozi-does-not-exist"); err != nil {
		t.Errorf("RevokeServiceAccount on missing SA should not error: %v", err)
	}
}
