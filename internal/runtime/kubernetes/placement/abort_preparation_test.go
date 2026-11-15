package placement

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	runtimekube "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (c *admissionClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, options ...client.PatchOption) error {
	if err := c.patchErr; err != nil {
		c.patchErr = nil
		return err
	}
	if err := c.Client.Patch(ctx, obj, patch, options...); err != nil {
		return err
	}
	if run, ok := obj.(*api.AgentRun); ok && c.reconcile != nil {
		c.reconcile(ctx, run)
	}
	return nil
}

// cancelReconcile is the controller honouring spec.cancel on a run that is
// not yet terminal.
func cancelReconcile(t *testing.T, kube *admissionClient) func(context.Context, *api.AgentRun) {
	return func(ctx context.Context, run *api.AgentRun) {
		if !run.Spec.Cancel || run.Status.Phase.IsTerminal() {
			return
		}
		run.Status.Phase = api.AgentRunPhaseCancelled
		if err := kube.Client.Status().Update(ctx, run); err != nil {
			t.Error(err)
		}
	}
}

// createSandbox is a reconcile that read the registration before it was
// cancelled: it creates the run's Pod and records its name. pending leaves
// the Pod unscheduled, with no container ever started.
func createSandbox(ctx context.Context, kube *admissionClient, run *api.AgentRun, pending bool) (*corev1.Pod, error) {
	run.Status.Phase = api.AgentRunPhaseRunning
	run.Status.Identity = api.AgentRunIdentity{SandboxName: runtimekube.RunName(run.UID), ServiceAccountName: "approved-worker"}
	if err := kube.Client.Status().Update(ctx, run); err != nil {
		return nil, err
	}
	pod := admittedPod(run)
	if pending {
		pod.Status = corev1.PodStatus{Phase: corev1.PodPending}
	} else {
		pod.Spec.NodeName = "node-a"
	}
	return pod, kube.Client.Create(ctx, pod)
}

// queuedPreparation leaves a launch's preparation as a coordinator sees it
// when its run waits under a concurrency cap: Prepare gave up waiting for the
// Pod, the registration is bound to the run and no Pod exists.
func queuedPreparation(t *testing.T) (*Placement, *admissionClient, session.Launch, []byte) {
	t.Helper()
	p, kube, _, l, data := setup(t)
	kube.queued = true
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if _, err := p.Prepare(ctx, l, data); err == nil {
		t.Fatal("queued preparation reported an incarnation")
	}
	r := registration(t, p, l)
	if r.Phase != registry.Bound || r.RunUID == "" || r.PodUID != "" {
		t.Fatalf("queued registration: %+v", r)
	}
	return p, kube, l, data
}

func registration(t *testing.T, p *Placement, l session.Launch) registry.Record {
	t.Helper()
	local, err := p.load(l)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.registry.Get(t.Context(), local.Reference)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func agentRun(t *testing.T, kube client.Client, l session.Launch) *api.AgentRun {
	t.Helper()
	var run api.AgentRun
	if err := kube.Get(t.Context(), client.ObjectKey{Namespace: fixtureNamespace, Name: runName(l)}, &run); err != nil {
		t.Fatal(err)
	}
	return &run
}

func sandboxPod(t *testing.T, kube client.Client, runUID string) (*corev1.Pod, bool) {
	t.Helper()
	var pod corev1.Pod
	err := kube.Get(t.Context(), client.ObjectKey{Namespace: runtimekube.AgentsNamespace, Name: runtimekube.RunName(types.UID(runUID))}, &pod)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return &pod, true
}

// assertRefusesPreparation checks a later Prepare of the aborted launch is
// refused before it creates anything.
func assertRefusesPreparation(t *testing.T, p *Placement, kube *admissionClient, l session.Launch, data []byte) {
	t.Helper()
	runs := kube.runCreates
	_, err := p.Prepare(t.Context(), l, data)
	var rejection *profile.Rejection
	if !errors.As(err, &rejection) || rejection.Code != profile.CodeConflict || kube.runCreates != runs {
		t.Fatalf("aborted preparation prepared again: %v, runs %d then %d", err, runs, kube.runCreates)
	}
}

// A run waiting under a concurrency cap has no Pod. Aborting its preparation
// cancels the registration and the run, waits for the controller to end the
// run, finds no Pod and succeeds; a replay succeeds again.
func TestAbortPreparationCancelsRunQueuedUnderCap(t *testing.T) {
	t.Parallel()
	p, kube, l, data := queuedPreparation(t)
	kube.reconcile = cancelReconcile(t, kube)
	if err := p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	r := registration(t, p, l)
	run := agentRun(t, kube, l)
	if r.Phase != registry.Cancelled || r.PodUID != "" || !run.Spec.Cancel || run.Status.Phase != api.AgentRunPhaseCancelled {
		t.Fatalf("aborted queued preparation: %+v, run %+v", r, run.Status)
	}
	if _, found := sandboxPod(t, kube, r.RunUID); found {
		t.Fatal("aborted queued preparation has a Pod")
	}
	if err := p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatalf("replay: %v", err)
	}
	assertRefusesPreparation(t, p, kube, l, data)
}

// A run that failed before its Pod existed, such as on an invalid runtime,
// is already terminal: the abort cancels the registration and needs nothing
// more from the controller.
func TestAbortPreparationOfRunFailedBeforePod(t *testing.T) {
	t.Parallel()
	p, kube, l, data := queuedPreparation(t)
	run := agentRun(t, kube, l)
	run.Status.Phase = api.AgentRunPhaseFailed
	if err := kube.Client.Status().Update(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	if err := p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	r := registration(t, p, l)
	if r.Phase != registry.Cancelled {
		t.Fatalf("registration after abort: %+v", r)
	}
	if run = agentRun(t, kube, l); run.Spec.Cancel || run.Status.Phase != api.AgentRunPhaseFailed {
		t.Fatalf("terminal run changed: %+v %+v", run.Spec, run.Status)
	}
	assertRefusesPreparation(t, p, kube, l, data)
}

// Until the controller has ended the run, a reconcile that read the
// registration before it was cancelled may still create the Pod, so no Pod is
// not yet proof. The abort reports that as unproven; once the late Pod exists
// a retry fences it.
func TestAbortPreparationWaitsForControllerAndFencesLatePod(t *testing.T) {
	t.Parallel()
	p, kube, l, _ := queuedPreparation(t)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if err := p.AbortPreparation(ctx, l); err == nil {
		t.Fatal("abort trusted an absent Pod before the controller ended the run")
	}
	r := registration(t, p, l)
	run := agentRun(t, kube, l)
	if r.Phase != registry.Cancelled || !run.Spec.Cancel {
		t.Fatalf("unproven abort did not cancel first: %+v %+v", r, run.Spec)
	}
	pod, err := createSandbox(t.Context(), kube, run, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	assertFencedLatePod(t, p, kube, l, pod)
}

// A Pod created while the abort runs, by a reconcile already past its checks,
// is no longer a preparation without a Pod: the abort fences it as Abort
// fences any Pod, from observed termination of the native container.
func TestAbortPreparationFencesPodCreatedDuringAbort(t *testing.T) {
	t.Parallel()
	p, kube, l, _ := queuedPreparation(t)
	var pod *corev1.Pod
	kube.reconcile = func(ctx context.Context, run *api.AgentRun) {
		if pod != nil {
			return
		}
		var err error
		if pod, err = createSandbox(ctx, kube, run, false); err != nil {
			t.Error(err)
		}
	}
	if err := p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	assertFencedLatePod(t, p, kube, l, pod)
}

func assertFencedLatePod(t *testing.T, p *Placement, kube *admissionClient, l session.Launch, pod *corev1.Pod) {
	t.Helper()
	r := registration(t, p, l)
	local, err := p.load(l)
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != registry.Cancelled || r.PodUID != string(pod.UID) || !local.WriterFenced {
		t.Fatalf("late Pod was not fenced: %+v %+v", r, local)
	}
	if current, found := sandboxPod(t, kube, r.RunUID); found && slices.Contains(current.Finalizers, runtimekube.ManagedWriterFenceFinalizer) {
		t.Fatalf("fenced Pod kept its writer fence: %+v", current.ObjectMeta)
	}
	if err = p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatalf("replay: %v", err)
	}
}

// A late Pod whose native container never reports a real termination stays
// unproven, including kubelet's synthetic ContainerStatusUnknown.
func TestAbortPreparationNeverTrustsUnknownTerminationOfLatePod(t *testing.T) {
	t.Parallel()
	p, kube, l, _ := queuedPreparation(t)
	kube.deferTermination = true
	run := agentRun(t, kube, l)
	pod, err := createSandbox(t.Context(), kube, run, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if err = p.AbortPreparation(ctx, l); err == nil {
		t.Fatal("running late Pod was reported fenced")
	}
	current, found := sandboxPod(t, kube, string(run.UID))
	if !found || current.DeletionTimestamp.IsZero() {
		t.Fatalf("late Pod was not deleted: %v", found)
	}
	current.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: 137, Reason: containerStatusUnknown,
		Message: "The container could not be located when the pod was terminated",
	}}
	if err = kube.Client.Status().Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if err = p.AbortPreparation(ctx, l); err == nil {
		t.Fatal("synthetic termination was reported fenced")
	}
	if local, err := p.load(l); err != nil || local.WriterFenced {
		t.Fatalf("synthetic termination retained a fence: %+v %v", local, err)
	}
	current, _ = sandboxPod(t, kube, string(run.UID))
	current.Status.ContainerStatuses[0].State = fixtureTermination(current.Status.ContainerStatuses[0].ContainerID, 137)
	if err = kube.Client.Status().Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	if err = p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	assertFencedLatePod(t, p, kube, l, pod)
}

// A late Pod that was never scheduled ran nothing, and once it is being
// deleted the API server never binds it to a node, so it is fenced without a
// termination record.
func TestAbortPreparationFencesUnscheduledLatePod(t *testing.T) {
	t.Parallel()
	p, kube, l, _ := queuedPreparation(t)
	kube.deferTermination = true
	pod, err := createSandbox(t.Context(), kube, agentRun(t, kube, l), true)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	assertFencedLatePod(t, p, kube, l, pod)
}

// A crash after the executor created the registration but before it saved
// the reference leaves only the run name; the abort finds and cancels it.
func TestAbortPreparationFindsRegistrationWhoseReferenceWasLost(t *testing.T) {
	t.Parallel()
	p, kube, l, data := queuedPreparation(t)
	local, err := p.load(l)
	if err != nil {
		t.Fatal(err)
	}
	ref := local.Reference
	local.Reference = registry.Reference{}
	if err = p.save(local); err != nil {
		t.Fatal(err)
	}
	kube.reconcile = cancelReconcile(t, kube)
	if err = p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	r, err := p.registry.Get(t.Context(), ref)
	if err != nil || r.Phase != registry.Cancelled || !agentRun(t, kube, l).Spec.Cancel {
		t.Fatalf("registration with lost reference: %+v %v", r, err)
	}
	assertRefusesPreparation(t, p, kube, l, data)
}

// A crash between cancelling the registration and cancelling the run is
// replayed: the registration is already cancelled and the retry finishes.
func TestAbortPreparationReplaysAfterCrashBetweenSteps(t *testing.T) {
	t.Parallel()
	p, kube, l, _ := queuedPreparation(t)
	kube.patchErr = errors.New("executor stopped")
	kube.reconcile = cancelReconcile(t, kube)
	if err := p.AbortPreparation(t.Context(), l); err == nil {
		t.Fatal("abort succeeded without cancelling the run")
	}
	if r := registration(t, p, l); r.Phase != registry.Cancelled {
		t.Fatalf("registration was not cancelled first: %+v", r)
	}
	if run := agentRun(t, kube, l); run.Spec.Cancel {
		t.Fatal("injected crash did not happen")
	}
	if err := p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	if run := agentRun(t, kube, l); !run.Spec.Cancel || run.Status.Phase != api.AgentRunPhaseCancelled {
		t.Fatalf("replayed abort: %+v %+v", run.Spec, run.Status)
	}
}

// A preparation this executor never received leaves a tombstone, so a late
// Prepare for that launch creates nothing.
func TestAbortPreparationOfUnseenLaunchRefusesLatePrepare(t *testing.T) {
	t.Parallel()
	p, kube, _, l, data := setup(t)
	if err := p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	assertRefusesPreparation(t, p, kube, l, data)
	if _, err := p.registry.Lookup(t.Context(), runName(l)); !errors.Is(err, registry.ErrGone) {
		t.Fatalf("late prepare registered the launch: %v", err)
	}
	if err := p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatalf("replay: %v", err)
	}
}

// A preparation that completed but whose reply was lost has a bound,
// restored Pod; the abort fences it as Abort does and seals its usage.
func TestAbortPreparationFencesPreparedPodWhoseReplyWasLost(t *testing.T) {
	t.Parallel()
	p, kube, _, l, data := setup(t)
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	r := registration(t, p, l)
	if r.Phase != registry.Cancelled || inc(r) != i || r.Receipt == nil {
		t.Fatalf("prepared registration after abort: %+v", r)
	}
	if _, found := sandboxPod(t, kube, r.RunUID); found {
		t.Fatal("fenced prepared Pod kept its writer fence")
	}
	if err = p.Activate(t.Context(), l, i); err == nil {
		t.Fatal("aborted preparation activated")
	}
}

// The coordinator cancels a pending Kubernetes destination whose run waits
// under a cap. Before the placement could abort a preparation this ended in
// unknown-stop, because Inspect had no incarnation to pass to Abort.
func TestCoordinatorCancelsKubernetesDestinationQueuedUnderCap(t *testing.T) {
	t.Parallel()
	p, kube, _, l, data := setup(t)
	kube.queued = true
	kube.reconcile = cancelReconcile(t, kube)
	l.Epoch = 1
	coordinator, err := session.Open(filepath.Join(t.TempDir(), "sessions"), map[string]session.Placement{l.Placement: p}, fixtureMax)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err = coordinator.Create(l, data, ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if r, err := coordinator.Recover(ctx, l.ExecutionID); err == nil || r.Phase != session.PhasePreparing {
		t.Fatalf("queued destination: %+v %v", r, err)
	}
	r, err := coordinator.Cancel(t.Context(), l.ExecutionID, uuid.NewString(), nil)
	if err != nil || r.Phase != session.PhaseCancelled {
		t.Fatalf("cancel of queued destination: %+v %v", r, err)
	}
	if run := agentRun(t, kube, l); run.Status.Phase != api.AgentRunPhaseCancelled {
		t.Fatalf("run after cancel: %+v", run.Status)
	}
}
