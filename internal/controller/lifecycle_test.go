package controller_test

import (
	"context"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/controller"
	k8sresources "github.com/andrewmccall/sproozi/internal/kubernetes"
)

func TestReleaseRetainsNetworkPolicyUntilSandboxTerminates(t *testing.T) {
	run := newRun("release-order")
	run.UID = types.UID("release-order-uid")
	run.Status.Phase = sprooziv1alpha1.AgentRunPhaseSucceeded
	run.Status.Identity = sprooziv1alpha1.AgentRunIdentity{ServiceAccountName: k8sresources.RunName(run.UID), SandboxName: k8sresources.RunName(run.UID)}
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: k8sresources.RunName(run.UID), Namespace: k8sresources.AgentsNamespace, Labels: map[string]string{k8sresources.ManagedByLabel: k8sresources.ManagedByValue, k8sresources.RunUIDLabel: string(run.UID)}}}
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(run, np).Build()
	sandbox := &k8sresources.FakeSandboxClient{Status: k8sresources.SandboxStatusRunning}
	r := &controller.AgentRunReconciler{Client: c, SandboxClient: sandbox}
	complete, err := r.ReconcileRelease(context.Background(), run)
	if err != nil || complete {
		t.Fatalf("release = (%v, %v), want incomplete without error", complete, err)
	}
	var retained networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(np), &retained); err != nil {
		t.Fatalf("NetworkPolicy was removed before Pod termination: %v", err)
	}
	sandbox.Status = k8sresources.SandboxStatusSucceeded
	complete, err = r.ReconcileRelease(context.Background(), run)
	if err != nil || !complete {
		t.Fatalf("terminated release = (%v, %v), want complete", complete, err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(np), &retained); err == nil {
		t.Fatal("NetworkPolicy retained after sandbox termination")
	}
}
