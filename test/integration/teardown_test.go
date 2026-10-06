package e2e_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/controller"
	k8sresources "github.com/andrewmccall/sproozi/internal/kubernetes"
)

func TestCancelledRunCleansUpRunScopedResources(t *testing.T) {
	c := e2eClient(t, e2eObjects()...)
	run := createRunViaWebhook(t, c, `{
  "id": "evt-200",
  "eventContext": {
    "alertname": "CrashLoopBackOff",
    "namespace": "sproozi-demo"
  }
}`)

	sandbox := &k8sresources.FakeSandboxClient{Status: k8sresources.SandboxStatusRunning}
	reconciler := &controller.AgentRunReconciler{
		Client:        c,
		Scheme:        e2eScheme(t),
		SandboxClient: sandbox,
	}

	reconcileRunNTimes(t, reconciler, run.Name, 4)

	var running sprooziv1alpha1.AgentRun
	if err := c.Get(context.Background(),
		client.ObjectKey{Name: run.Name,
			Namespace: e2eNamespace},
		&running); err != nil {
		t.Fatalf("Get running AgentRun: %v", err)
	}
	if running.Status.Phase != sprooziv1alpha1.AgentRunPhaseRunning {
		t.Fatalf("expected Running before cancellation, got %q", running.Status.Phase)
	}

	saName := running.Status.Identity.ServiceAccountName
	if saName == "" {
		t.Fatal("expected service account name to be provisioned")
	}

	running.Spec.Cancel = true
	if err := c.Update(context.Background(), &running); err != nil {
		t.Fatalf("Update AgentRun cancel: %v", err)
	}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: run.Name, Namespace: e2eNamespace},
	}); err != nil {
		t.Fatalf("Reconcile cancel transition: %v", err)
	}
	// The lifecycle releaser waits for confirmed sandbox termination before
	// revoking the remaining run-scoped resources.
	sandbox.Status = k8sresources.SandboxStatusSucceeded
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: run.Name, Namespace: e2eNamespace},
	}); err != nil {
		t.Fatalf("Reconcile terminal cleanup: %v", err)
	}

	var cleaned sprooziv1alpha1.AgentRun
	if err := c.Get(context.Background(),
		client.ObjectKey{Name: run.Name,
			Namespace: e2eNamespace},
		&cleaned); err != nil {
		t.Fatalf("Get cleaned AgentRun: %v", err)
	}
	if cleaned.Status.Identity.ServiceAccountName != "" || cleaned.Status.Identity.SandboxName != "" {
		t.Fatalf("expected run identity to be cleared, got %#v", cleaned.Status.Identity)
	}

	var sa corev1.ServiceAccount
	if err := c.Get(context.Background(),
		client.ObjectKey{Name: saName,
			Namespace: k8sresources.AgentsNamespace},
		&sa); !apierrors.IsNotFound(err) {
		t.Fatalf("expected service account cleanup, got err=%v", err)
	}

	var np networkingv1.NetworkPolicy
	if err := c.Get(context.Background(),
		client.ObjectKey{Name: saName,
			Namespace: k8sresources.AgentsNamespace},
		&np); !apierrors.IsNotFound(err) {
		t.Fatalf("expected network policy cleanup, got err=%v", err)
	}

	if len(sandbox.Deleted) != 1 {
		t.Fatalf("expected one sandbox delete, got %v", sandbox.Deleted)
	}
}
