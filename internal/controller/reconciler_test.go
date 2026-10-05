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

package controller_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/utils/ptr"

	corev1 "k8s.io/api/core/v1"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/controller"

	k8sresources "github.com/andrewmccall/sproozi/internal/kubernetes"
)

// testNS is the namespace used across reconciler unit tests.
const testNS = "default"

// newScheme builds a runtime scheme with the sproozi types plus the built-in
// Kubernetes types (corev1/rbacv1/networkingv1) needed by the identity provisioning
// helpers in internal/kubernetes.
func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = sprooziv1alpha1.AddToScheme(s)
	_ = clientgoscheme.AddToScheme(s)
	_ = networkingv1.AddToScheme(s)
	return s
}

// fixtures creates the prerequisite AgentRuntime, AgentPolicy, and AgentTemplate.
func fixtures() []client.Object {
	allowPrivEsc, readOnly, nonRoot := false, true, true
	rt := &sprooziv1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{Name: "codex", Namespace: testNS},
		Spec: sprooziv1alpha1.AgentRuntimeSpec{

			EphemeralWorkspace: sprooziv1alpha1.EphemeralWorkspaceSpec{SizeLimit: resource.MustParse("2Gi")},

			PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "ghcr.io/openai/codex@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			}, SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &allowPrivEsc, ReadOnlyRootFilesystem: &readOnly, RunAsNonRoot: &nonRoot, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}}}, SecurityContext: &corev1.
						PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)),
				FSGroup: ptr.To(int64(65532)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			}},
			WorkloadContainers: []string{"agent"}, GatewayEndpoint: "https://sproozi-gateway.sproozi-system.svc:8443",
		},
	}
	pol := &sprooziv1alpha1.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "sre", Namespace: testNS, Generation: 1},
		Spec: sprooziv1alpha1.AgentPolicySpec{
			AllowedCapabilities: []sprooziv1alpha1.CapabilityKind{
				sprooziv1alpha1.CapabilityKubernetesRead,
				sprooziv1alpha1.CapabilityGitHubPullRequest,
			},
			KubernetesRead:       sprooziv1alpha1.KubernetesReadScope{Namespaces: []string{"demo"}, Resources: []sprooziv1alpha1.KubernetesReadResource{"pods"}},
			GitHubPullRequest:    sprooziv1alpha1.GitHubPullRequestScope{Repositories: []string{"andrewmccall/home-ops"}, AllowedBaseBranches: []string{"main"}},
			EgressProfiles:       []string{"go-modules"},
			Budgets:              map[sprooziv1alpha1.CapabilityKind]sprooziv1alpha1.EndpointBudget{sprooziv1alpha1.CapabilityModelInference: {MaxUnits: 100_000, MaxCostMicros: 500_000}},
			ResourceBounds:       sprooziv1alpha1.ResourceBounds{Max: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}},
			MaxExecutionDuration: metav1.Duration{Duration: time.Hour},
			RetentionTTL:         metav1.Duration{Duration: 30 * 24 * time.Hour},
		},
	}
	tmpl := &sprooziv1alpha1.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "sre-remediation", Namespace: testNS},
		Spec: sprooziv1alpha1.AgentTemplateSpec{
			RuntimeRef:     sprooziv1alpha1.AgentRuntimeReference{Name: "codex"},
			PolicyRef:      sprooziv1alpha1.AgentPolicyReference{Name: "sre"},
			Instructions:   "Investigate and fix.",
			EgressProfiles: []string{"go-modules"},
		},
	}
	return []client.Object{rt, pol, tmpl}
}

func newRun(name string) *sprooziv1alpha1.AgentRun {
	return &sprooziv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNS,
			UID:       types.UID("test-uid-" + name),
		},
		Spec: sprooziv1alpha1.AgentRunSpec{
			TemplateRef:  sprooziv1alpha1.AgentTemplateReference{Name: "sre-remediation"},
			Task:         "Investigate crash.",
			Capabilities: []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityKubernetesRead},
		},
	}
}

func reconcileRun(t *testing.T, c client.Client, name string) (ctrl.Result, error) {
	t.Helper()
	r := &controller.AgentRunReconciler{Client: c, Scheme: newScheme()}
	return r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: testNS},
	})
}

// newReconcilerWithSandbox returns a reconciler wired with the given SandboxClient.
func newReconcilerWithSandbox(sc k8sresources.SandboxClient, objs ...client.Object) (*controller.AgentRunReconciler, client.Client) {
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&sprooziv1alpha1.AgentRun{}).
		Build()
	r := &controller.AgentRunReconciler{
		Client:        c,
		Scheme:        newScheme(),
		SandboxClient: sc,
	}
	return r, c
}

// driveToRunning runs 3 reconcile cycles to get a run from "" → Queued → Admitted → Running.
// The sandbox is NOT yet created after this call; the first handleRunning invocation
// (reconcile 4) is what calls ensureSandbox.
func driveToRunning(t *testing.T, r *controller.AgentRunReconciler, _ client.Client, name string) {
	t.Helper()
	for i := range 3 {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: testNS},
		}); err != nil {
			t.Fatalf("reconcile %d error: %v", i+1, err)
		}
	}
}

func getRun(t *testing.T, c client.Client, name string) sprooziv1alpha1.AgentRun {
	t.Helper()
	var run sprooziv1alpha1.AgentRun
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: testNS}, &run); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	return run
}

func getPhase(t *testing.T, c client.Client, name string) sprooziv1alpha1.AgentRunPhase {
	t.Helper()
	return getRun(t, c, name).Status.Phase
}

func TestReconcilerNewRunBecomesQueued(t *testing.T) {
	t.Parallel()

	objs := append(fixtures(), newRun("run1"))
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&sprooziv1alpha1.AgentRun{}).
		Build()

	if _, err := reconcileRun(t, c, "run1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if phase := getPhase(t, c, "run1"); phase != sprooziv1alpha1.AgentRunPhaseQueued {
		t.Errorf("phase = %q, want %q", phase, sprooziv1alpha1.AgentRunPhaseQueued)
	}
	queued := getRun(t, c, "run1")
	if !controllerutil.ContainsFinalizer(&queued, "sproozi.com/agentrun-cleanup") {
		t.Fatal("active AgentRun must retain cleanup finalizer")
	}
}

func TestReconcilerQueuedRunAdmittedWhenNoActiveRun(t *testing.T) {
	t.Parallel()

	run := newRun("run1")
	run.Status.Phase = sprooziv1alpha1.AgentRunPhaseQueued
	objs := append(fixtures(), run)
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&sprooziv1alpha1.AgentRun{}).
		Build()

	if _, err := reconcileRun(t, c, "run1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	got := getRun(t, c, "run1")
	if got.Status.Phase != sprooziv1alpha1.AgentRunPhaseAdmitted {
		t.Errorf("phase = %q, want %q", got.Status.Phase, sprooziv1alpha1.AgentRunPhaseAdmitted)
	}
	if got.Status.StartedAt == nil {
		t.Error("StartedAt should be set when run is admitted")
	}
}

func TestReconcilerQueuedRunStaysQueuedWhenActiveRunExists(t *testing.T) {
	t.Parallel()

	run1 := newRun("run1")
	run1.Status.Phase = sprooziv1alpha1.AgentRunPhaseAdmitted
	run2 := newRun("run2")
	run2.Status.Phase = sprooziv1alpha1.AgentRunPhaseQueued
	objs := append(fixtures(), run1, run2)
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&sprooziv1alpha1.AgentRun{}).
		Build()

	if _, err := reconcileRun(t, c, "run2"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if phase := getPhase(t, c, "run2"); phase != sprooziv1alpha1.AgentRunPhaseQueued {
		t.Errorf("phase = %q, want %q", phase, sprooziv1alpha1.AgentRunPhaseQueued)
	}
}

func TestReconcilerQueuedRunFailsWhenTemplateMissing(t *testing.T) {
	t.Parallel()

	// Only runtime and policy; no template
	rt := fixtures()[0]
	pol := fixtures()[1]
	run := newRun("run1")
	run.Status.Phase = sprooziv1alpha1.AgentRunPhaseQueued
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(rt, pol, run).
		WithStatusSubresource(&sprooziv1alpha1.AgentRun{}).
		Build()

	if _, err := reconcileRun(t, c, "run1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if phase := getPhase(t, c, "run1"); phase != sprooziv1alpha1.AgentRunPhaseFailed {
		t.Errorf("phase = %q, want %q", phase, sprooziv1alpha1.AgentRunPhaseFailed)
	}
}

func TestReconcilerQueuedRunFailsWhenPolicyMissing(t *testing.T) {
	t.Parallel()

	rt := fixtures()[0]
	// Template references "sre" policy, which is not present.
	tmpl := fixtures()[2]
	run := newRun("run1")
	run.Status.Phase = sprooziv1alpha1.AgentRunPhaseQueued
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(rt, tmpl, run).
		WithStatusSubresource(&sprooziv1alpha1.AgentRun{}).
		Build()

	if _, err := reconcileRun(t, c, "run1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if phase := getPhase(t, c, "run1"); phase != sprooziv1alpha1.AgentRunPhaseFailed {
		t.Errorf("phase = %q, want %q", phase, sprooziv1alpha1.AgentRunPhaseFailed)
	}
}

func TestReconcilerQueuedRunFailsWhenRuntimeMissing(t *testing.T) {
	t.Parallel()

	pol := fixtures()[1]
	tmpl := fixtures()[2]
	// No runtime.
	run := newRun("run1")
	run.Status.Phase = sprooziv1alpha1.AgentRunPhaseQueued
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(pol, tmpl, run).
		WithStatusSubresource(&sprooziv1alpha1.AgentRun{}).
		Build()

	if _, err := reconcileRun(t, c, "run1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if phase := getPhase(t, c, "run1"); phase != sprooziv1alpha1.AgentRunPhaseFailed {
		t.Errorf("phase = %q, want %q", phase, sprooziv1alpha1.AgentRunPhaseFailed)
	}
}

func TestReconcilerQueuedRunFailsWhenCapabilityDenied(t *testing.T) {
	t.Parallel()

	run := newRun("run1")
	run.Status.Phase = sprooziv1alpha1.AgentRunPhaseQueued
	// Request a capability not in the policy
	run.Spec.Capabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityNetworkEgress}
	objs := append(fixtures(), run)
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&sprooziv1alpha1.AgentRun{}).
		Build()

	if _, err := reconcileRun(t, c, "run1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if phase := getPhase(t, c, "run1"); phase != sprooziv1alpha1.AgentRunPhaseFailed {
		t.Errorf("phase = %q, want %q", phase, sprooziv1alpha1.AgentRunPhaseFailed)
	}
}

func TestReconcilerQueuedRunCancelledWhenCancelSet(t *testing.T) {
	t.Parallel()

	run := newRun("run1")
	run.Status.Phase = sprooziv1alpha1.AgentRunPhaseQueued
	run.Spec.Cancel = true
	objs := append(fixtures(), run)
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&sprooziv1alpha1.AgentRun{}).
		Build()

	if _, err := reconcileRun(t, c, "run1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if phase := getPhase(t, c, "run1"); phase != sprooziv1alpha1.AgentRunPhaseCancelled {
		t.Errorf("phase = %q, want %q", phase, sprooziv1alpha1.AgentRunPhaseCancelled)
	}
}

func TestReconcilerAdmittedRunCancelledWhenCancelSet(t *testing.T) {
	t.Parallel()

	run := newRun("run1")
	startedAt := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	run.Status.Phase = sprooziv1alpha1.AgentRunPhaseAdmitted
	run.Status.StartedAt = &startedAt
	run.Spec.Cancel = true
	objs := append(fixtures(), run)
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&sprooziv1alpha1.AgentRun{}).
		Build()

	if _, err := reconcileRun(t, c, "run1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if phase := getPhase(t, c, "run1"); phase != sprooziv1alpha1.AgentRunPhaseCancelled {
		t.Errorf("phase = %q, want %q", phase, sprooziv1alpha1.AgentRunPhaseCancelled)
	}
}

func TestReconcilerAdmittedRunTimedOut(t *testing.T) {
	t.Parallel()

	run := newRun("run1")
	// StartedAt was 2 hours ago; policy MaxExecutionDuration is 1 hour.
	twoHoursAgo := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	run.Status.Phase = sprooziv1alpha1.AgentRunPhaseAdmitted
	run.Status.StartedAt = &twoHoursAgo
	objs := append(fixtures(), run)
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&sprooziv1alpha1.AgentRun{}).
		Build()

	if _, err := reconcileRun(t, c, "run1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if phase := getPhase(t, c, "run1"); phase != sprooziv1alpha1.AgentRunPhaseTimedOut {
		t.Errorf("phase = %q, want %q", phase, sprooziv1alpha1.AgentRunPhaseTimedOut)
	}
}

func TestReconcilerRunningRunTimedOut(t *testing.T) {
	t.Parallel()

	run := newRun("run1")
	run.Status.Phase = sprooziv1alpha1.AgentRunPhaseRunning
	// StartedAt was 2 hours ago; policy MaxExecutionDuration is 1 hour.
	twoHoursAgo := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	run.Status.StartedAt = &twoHoursAgo
	objs := append(fixtures(), run)
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&sprooziv1alpha1.AgentRun{}).
		Build()

	if _, err := reconcileRun(t, c, "run1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if phase := getPhase(t, c, "run1"); phase != sprooziv1alpha1.AgentRunPhaseTimedOut {
		t.Errorf("phase = %q, want %q", phase, sprooziv1alpha1.AgentRunPhaseTimedOut)
	}
}

func TestReconcilerRunningRunCancelledWhenCancelSet(t *testing.T) {
	t.Parallel()

	run := newRun("run1")
	run.Status.Phase = sprooziv1alpha1.AgentRunPhaseRunning
	run.Spec.Cancel = true
	startedAt := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	run.Status.StartedAt = &startedAt
	objs := append(fixtures(), run)
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&sprooziv1alpha1.AgentRun{}).
		Build()

	if _, err := reconcileRun(t, c, "run1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if phase := getPhase(t, c, "run1"); phase != sprooziv1alpha1.AgentRunPhaseCancelled {
		t.Errorf("phase = %q, want %q", phase, sprooziv1alpha1.AgentRunPhaseCancelled)
	}
}

func TestReconcilerTerminalRunDeletedAfterTTL(t *testing.T) {
	t.Parallel()

	run := newRun("run1")
	run.Status.Phase = sprooziv1alpha1.AgentRunPhaseSucceeded
	// Completed 31 days ago; TTL is 30 days.
	past := metav1.NewTime(time.Now().Add(-31 * 24 * time.Hour))
	run.Status.CompletedAt = &past
	objs := append(fixtures(), run)
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&sprooziv1alpha1.AgentRun{}).
		Build()

	if _, err := reconcileRun(t, c, "run1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	var got sprooziv1alpha1.AgentRun
	err := c.Get(context.Background(), types.NamespacedName{Name: "run1", Namespace: testNS}, &got)
	if err == nil {
		t.Errorf("Get() after TTL = nil error (run still exists), want not-found")
	}
}

func TestReconcilerTerminalRunNotDeletedBeforeTTL(t *testing.T) {
	t.Parallel()

	run := newRun("run1")
	run.Status.Phase = sprooziv1alpha1.AgentRunPhaseSucceeded
	// Completed only 1 hour ago; TTL is 30 days.
	recent := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	run.Status.CompletedAt = &recent
	objs := append(fixtures(), run)
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&sprooziv1alpha1.AgentRun{}).
		Build()

	if _, err := reconcileRun(t, c, "run1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if phase := getPhase(t, c, "run1"); phase != sprooziv1alpha1.AgentRunPhaseSucceeded {
		t.Errorf("phase = %q, want Succeeded (run should still exist)", phase)
	}
}

// TestReconcilerTerminalRunNotRewrittenWhenTemplateMissing validates the C1 fix:
// a terminal run whose template is later deleted must NOT be transitioned to Failed.
func TestReconcilerTerminalRunNotRewrittenWhenTemplateMissing(t *testing.T) {
	t.Parallel()

	run := newRun("run1")
	run.Status.Phase = sprooziv1alpha1.AgentRunPhaseSucceeded
	// CompletedAt is recent enough that TTL has not elapsed.
	recent := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	run.Status.CompletedAt = &recent
	// No template, policy, or runtime — they have been deleted.
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(run).
		WithStatusSubresource(&sprooziv1alpha1.AgentRun{}).
		Build()

	if _, err := reconcileRun(t, c, "run1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if phase := getPhase(t, c, "run1"); phase != sprooziv1alpha1.AgentRunPhaseSucceeded {
		t.Errorf("phase = %q after template deletion; want Succeeded (terminal runs must not be rewritten)", phase)
	}
}

func TestReconcilerIdentityCreatedOnAdmit(t *testing.T) {
	t.Parallel()

	run := newRun("run-identity-1")
	objs := append(fixtures(), run)
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&sprooziv1alpha1.AgentRun{}).
		Build()

	// First reconcile: "" → Queued
	if _, err := reconcileRun(t, c, "run-identity-1"); err != nil {
		t.Fatalf("reconcile 1 (init) error: %v", err)
	}
	// Second reconcile: Queued → Admitted
	if _, err := reconcileRun(t, c, "run-identity-1"); err != nil {
		t.Fatalf("reconcile 2 (admit) error: %v", err)
	}
	// Third reconcile: Admitted → provision identity → Running
	if _, err := reconcileRun(t, c, "run-identity-1"); err != nil {
		t.Fatalf("reconcile 3 (provision) error: %v", err)
	}

	got := getRun(t, c, "run-identity-1")
	if got.Status.Phase != sprooziv1alpha1.AgentRunPhaseRunning {
		t.Fatalf("expected Running after identity provision, got %q", got.Status.Phase)
	}
	if got.Status.Identity.ServiceAccountName == "" {
		t.Error("Identity.ServiceAccountName must be set after provision")
	}
}

func TestReconcilerIdentityRevokedOnTerminal(t *testing.T) {
	t.Parallel()

	run := newRun("run-identity-2")
	objs := append(fixtures(), run)
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&sprooziv1alpha1.AgentRun{}).
		Build()

	// Drive to Running.
	if _, err := reconcileRun(t, c, "run-identity-2"); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if _, err := reconcileRun(t, c, "run-identity-2"); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if _, err := reconcileRun(t, c, "run-identity-2"); err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}

	// Cancel the run.
	running := getRun(t, c, "run-identity-2")
	running.Spec.Cancel = true
	if err := c.Update(context.Background(), &running); err != nil {
		t.Fatalf("Update cancel: %v", err)
	}
	if _, err := reconcileRun(t, c, "run-identity-2"); err != nil {
		t.Fatalf("reconcile cancel: %v", err)
	}

	term := getRun(t, c, "run-identity-2")
	if term.Status.Phase != sprooziv1alpha1.AgentRunPhaseCancelled {
		t.Fatalf("expected Cancelled, got %q", term.Status.Phase)
	}

	// Terminal handler runs: revokes identity.
	if _, err := reconcileRun(t, c, "run-identity-2"); err != nil {
		t.Fatalf("reconcile terminal: %v", err)
	}

	cleaned := getRun(t, c, "run-identity-2")
	if cleaned.Status.Identity.ServiceAccountName != "" {
		t.Error("Identity.ServiceAccountName should be cleared after terminal cleanup")
	}
}

func TestReconcilerSandboxCreatedOnRunning(t *testing.T) {
	t.Parallel()
	sc := &k8sresources.FakeSandboxClient{Status: k8sresources.SandboxStatusRunning}
	run := newRun("run-sandbox-1")
	r, c := newReconcilerWithSandbox(sc, append(fixtures(), run)...)

	driveToRunning(t, r, c, "run-sandbox-1")

	// After driveToRunning, we're at Running. Now reconcile once more to trigger
	// the sandbox creation path in handleRunning.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "run-sandbox-1", Namespace: testNS},
	}); err != nil {
		t.Fatalf("reconcile (running) error: %v", err)
	}

	got := getRun(t, c, "run-sandbox-1")
	if got.Status.Phase != sprooziv1alpha1.AgentRunPhaseRunning {
		t.Fatalf("expected Running, got %q", got.Status.Phase)
	}
	if got.Status.Identity.SandboxName == "" {
		t.Error("SandboxName must be set after Running")
	}
	if len(sc.Ensured) == 0 {
		t.Error("Ensure must have been called at least once")
	}
}

func TestReconcilerSandboxSucceededTransitionsToSucceeded(t *testing.T) {
	t.Parallel()

	sc := &k8sresources.FakeSandboxClient{
		Status:   k8sresources.SandboxStatusSucceeded,
		ExitCode: 0,
	}
	run := newRun("run-sandbox-2")
	r, c := newReconcilerWithSandbox(sc, append(fixtures(), run)...)

	driveToRunning(t, r, c, "run-sandbox-2")
	// Reconcile while Succeeded
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "run-sandbox-2", Namespace: testNS},
	}); err != nil {
		t.Fatalf("reconcile (succeeded) error: %v", err)
	}

	got := getRun(t, c, "run-sandbox-2")
	if got.Status.Phase != sprooziv1alpha1.AgentRunPhaseSucceeded {
		t.Fatalf("expected Succeeded, got %q", got.Status.Phase)
	}
}

func TestReconcilerSandboxFailedTransitionsToFailed(t *testing.T) {
	t.Parallel()
	sc := &k8sresources.FakeSandboxClient{
		Status:   k8sresources.SandboxStatusFailed,
		ExitCode: 1,
	}
	run := newRun("run-sandbox-3")
	r, c := newReconcilerWithSandbox(sc, append(fixtures(), run)...)

	driveToRunning(t, r, c, "run-sandbox-3")
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "run-sandbox-3", Namespace: testNS},
	}); err != nil {
		t.Fatalf("reconcile (failed) error: %v", err)
	}

	got := getRun(t, c, "run-sandbox-3")
	if got.Status.Phase != sprooziv1alpha1.AgentRunPhaseFailed {
		t.Fatalf("expected Failed, got %q", got.Status.Phase)
	}
}

func TestReconcilerSandboxWithoutExitCodeTransitionsToFailed(t *testing.T) {
	sc := &k8sresources.FakeSandboxClient{
		Status:    k8sresources.SandboxStatusFailed,
		ResultErr: errors.New("agent container has not terminated"),
	}
	run := newRun("run-sandbox-no-exit")
	r, c := newReconcilerWithSandbox(sc, append(fixtures(), run)...)
	driveToRunning(t, r, c, run.Name)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: run.Name, Namespace: testNS}}); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	got := getRun(t, c, run.Name)
	if got.Status.Phase != sprooziv1alpha1.AgentRunPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
}

func TestReconcilerSandboxUnknownTransitionsToFailed(t *testing.T) {
	t.Parallel()
	sc := &k8sresources.FakeSandboxClient{Status: k8sresources.SandboxStatusUnknown}
	run := newRun("run-sandbox-4")
	r, c := newReconcilerWithSandbox(sc, append(fixtures(), run)...)

	driveToRunning(t, r, c, "run-sandbox-4")
	// First reconcile of Running: creates sandbox, gets Unknown → transitions to Failed
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "run-sandbox-4", Namespace: testNS},
	}); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	got := getRun(t, c, "run-sandbox-4")
	if got.Status.Phase != sprooziv1alpha1.AgentRunPhaseFailed {
		t.Fatalf("expected Failed on Unknown sandbox, got %q", got.Status.Phase)
	}
}

func TestReconcilerSandboxDeletedOnTerminal(t *testing.T) {
	t.Parallel()
	sc := &k8sresources.FakeSandboxClient{
		Status:   k8sresources.SandboxStatusSucceeded,
		ExitCode: 0,
	}
	run := newRun("run-sandbox-5")
	r, c := newReconcilerWithSandbox(sc, append(fixtures(), run)...)

	driveToRunning(t, r, c, "run-sandbox-5")
	// Reconcile: Running → Succeeded (Sandbox succeeded)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "run-sandbox-5", Namespace: testNS}}); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	// Reconcile: terminal handler revokes + deletes sandbox
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "run-sandbox-5", Namespace: testNS}}); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	if len(sc.Deleted) == 0 {
		t.Error("Delete must have been called at least once for terminal run")
	}
}
