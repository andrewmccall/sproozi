package controller_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/controller"
	k8sresources "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
)

func TestReleaseRecoversSandboxAndRetainsUncertainContainment(t *testing.T) {
	for _, missingStatus := range []bool{false, true} {
		for _, phase := range []corev1.PodPhase{"", corev1.PodUnknown, corev1.PodPending, corev1.PodRunning} {
			t.Run(string(phase)+map[bool]string{true: "/recovered", false: "/recorded"}[missingStatus], func(t *testing.T) {
				ctx := context.Background()
				run := newRun("release-recovered")
				run.Status.Phase = sprooziv1alpha1.AgentRunPhaseCancelled
				name := k8sresources.RunName(run.UID)
				if !missingStatus {
					run.Status.Identity = sprooziv1alpha1.AgentRunIdentity{ServiceAccountName: name, SandboxName: name}
				}
				labels := map[string]string{k8sresources.ManagedByLabel: k8sresources.ManagedByValue, k8sresources.RunUIDLabel: string(run.UID)}
				np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: k8sresources.AgentsNamespace, Labels: labels}}
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: k8sresources.AgentsNamespace, UID: "pod-incarnation", Labels: labels, Finalizers: []string{k8sresources.ManagedWriterFenceFinalizer}}, Status: corev1.PodStatus{Phase: phase}}
				c := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).WithObjects(run, np, pod).Build()
				r := &controller.AgentRunReconciler{Client: c, APIReader: c, SandboxClient: k8sresources.NewPodSandboxClient(c)}
				for range 2 {
					complete, err := r.ReconcileRelease(ctx, run)
					if err != nil || complete {
						t.Fatalf("uncertain release=(%v,%v)", complete, err)
					}
					if err := c.Get(ctx, client.ObjectKeyFromObject(np), &networkingv1.NetworkPolicy{}); err != nil {
						t.Fatalf("containment lost: %v", err)
					}
				}
				var retained corev1.Pod
				if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &retained); err != nil {
					t.Fatal(err)
				}
				if retained.DeletionTimestamp.IsZero() || len(retained.Finalizers) != 1 {
					t.Fatalf("writer fence not retained: %+v", retained.ObjectMeta)
				}
				retained.Status.Phase = corev1.PodSucceeded
				if err := c.Status().Update(ctx, &retained); err != nil {
					t.Fatal(err)
				}
				complete, err := r.ReconcileRelease(ctx, run)
				if err != nil || !complete {
					t.Fatalf("stopped release=(%v,%v)", complete, err)
				}
				if err := c.Get(ctx, client.ObjectKeyFromObject(np), &networkingv1.NetworkPolicy{}); !apierrors.IsNotFound(err) {
					t.Fatalf("stopped containment cleanup: %v", err)
				}
			})
		}
	}
}

func TestReleaseDoesNotAdoptDifferentRunAndCleansAbsentRun(t *testing.T) {
	ctx := context.Background()
	run := newRun("release-foreign")
	name := k8sresources.RunName(run.UID)
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: k8sresources.AgentsNamespace, Labels: map[string]string{k8sresources.ManagedByLabel: k8sresources.ManagedByValue, k8sresources.RunUIDLabel: string(run.UID)}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: k8sresources.AgentsNamespace, Labels: map[string]string{k8sresources.ManagedByLabel: k8sresources.ManagedByValue, k8sresources.RunUIDLabel: "different-run"}}}
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(run, np, pod).Build()
	r := &controller.AgentRunReconciler{Client: c, APIReader: c, SandboxClient: k8sresources.NewPodSandboxClient(c)}
	complete, err := r.ReconcileRelease(ctx, run)
	if err == nil || complete {
		t.Fatalf("foreign recovery=(%v,%v)", complete, err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
		t.Fatalf("foreign Pod removed: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(np), &networkingv1.NetworkPolicy{}); err != nil {
		t.Fatalf("containment lost: %v", err)
	}
	if err := c.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		complete, err = r.ReconcileRelease(ctx, run)
		if err != nil || !complete {
			t.Fatalf("absent release=(%v,%v)", complete, err)
		}
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(np), &networkingv1.NetworkPolicy{}); !apierrors.IsNotFound(err) {
		t.Fatalf("absent cleanup: %v", err)
	}
}
