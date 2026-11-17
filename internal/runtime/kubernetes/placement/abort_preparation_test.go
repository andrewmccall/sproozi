package placement

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
	runtimekube "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/sandboxpod"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	return createSandboxWith(ctx, kube, run, func(pod *corev1.Pod) {
		if pending {
			pod.Status = corev1.PodStatus{Phase: corev1.PodPending}
		} else {
			pod.Spec.NodeName = fixtureNode
		}
	})
}

const fixtureNode = "node-a"

// createSandboxWith is createSandbox with the Pod shaped by mutate.
func createSandboxWith(ctx context.Context, kube *admissionClient, run *api.AgentRun, mutate func(*corev1.Pod)) (*corev1.Pod, error) {
	run.Status.Phase = api.AgentRunPhaseRunning
	run.Status.Identity = api.AgentRunIdentity{SandboxName: runtimekube.RunName(run.UID), ServiceAccountName: fixtureWorker}
	if err := kube.Client.Status().Update(ctx, run); err != nil {
		return nil, err
	}
	pod := admittedPod(run)
	mutate(pod)
	return pod, kube.Client.Create(ctx, pod)
}

type fixtureAuthorizer struct{}

const fixtureToken = "fixture-home-principal-token-of-32-bytes-or-more"

func (fixtureAuthorizer) Authorize(_ context.Context, token string) error {
	if token != fixtureToken {
		return profile.ErrUnauthenticated
	}
	return nil
}

// serveProfile serves the placement over the placement protocol, as the
// executor does, and returns a home's client with the given request timeout.
func serveProfile(t *testing.T, p *Placement, l session.Launch, timeout time.Duration) *profile.Client {
	t.Helper()
	h, err := profile.NewHandler(profile.Config{Harness: l.Harness, Principal: p.config.Principal, Placement: l.Placement,
		InstructionDigest: l.InstructionDigest, Allowances: l.Allowances, MaxDuration: time.Hour, MaxCheckpointBytes: fixtureMax,
		Authorizer: fixtureAuthorizer{}, Backend: p})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewUnstartedServer(h)
	s.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	s.StartTLS()
	t.Cleanup(s.Close)
	roots := x509.NewCertPool()
	roots.AddCert(s.Certificate())
	c, err := profile.Connect(profile.Connection{Origin: s.URL, Token: fixtureToken, Roots: roots, MaxCheckpointBytes: fixtureMax, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
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
	var contradicted *contradiction
	if err != nil && !errors.As(err, &contradicted) {
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
// refused before it creates anything: as a start that can never succeed,
// so a coordinator whose end of it was unproven replays the abort (ADR
// 0067), and as a conflict for another launch of the same allocation.
func assertRefusesPreparation(t *testing.T, p *Placement, kube *admissionClient, l session.Launch, data []byte) {
	t.Helper()
	runs := kube.runCreates
	_, err := p.Prepare(t.Context(), l, data)
	if !errors.Is(err, session.ErrStartRefused) || kube.runCreates != runs {
		t.Fatalf("aborted preparation prepared again: %v, runs %d then %d", err, runs, kube.runCreates)
	}
	other := l
	other.OperationID = uuid.NewString()
	_, err = p.Prepare(t.Context(), other, data)
	var rejection *profile.Rejection
	if !errors.As(err, &rejection) || rejection.Code != profile.CodeConflict || kube.runCreates != runs {
		t.Fatalf("another launch of the aborted allocation: %v, runs %d then %d", err, runs, kube.runCreates)
	}
	// Every caller's abort succeeded, yet Inspect still says preparing: only
	// AbortPreparation proves the end (ADR 0067).
	if o, err := p.Inspect(t.Context(), l); err != nil || o.State != session.PlacementPreparing {
		t.Fatalf("Inspect of the settled aborted preparation: %+v %v", o, err)
	}
	if _, err = p.Inspect(t.Context(), other); !errors.As(err, &rejection) || rejection.Code != profile.CodeConflict {
		t.Fatalf("Inspect of another launch of the aborted allocation: %v", err)
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
	p.abortWait = 300 * time.Millisecond
	if err = p.AbortPreparation(t.Context(), l); !errors.Is(err, errWriterFenceUnknown) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("running late Pod: %v", err)
	}
	current, found := sandboxPod(t, kube, string(run.UID))
	if !found || current.DeletionTimestamp.IsZero() {
		t.Fatalf("late Pod was not deleted: %v", found)
	}
	current.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: 137, Reason: containerStatusUnknown,
		Message: unknownTerminationMessage,
	}}
	if err = kube.Client.Status().Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	if err = p.AbortPreparation(t.Context(), l); !errors.Is(err, errWriterFenceUnknown) {
		t.Fatalf("synthetic termination: %v", err)
	}
	if local, err := p.load(l); err != nil || local.WriterFenced {
		t.Fatalf("synthetic termination retained a fence: %+v %v", local, err)
	}
	current, _ = sandboxPod(t, kube, string(run.UID))
	current.Status.ContainerStatuses[0].State = fixtureTermination(current.Status.ContainerStatuses[0].ContainerID, 137)
	if err = kube.Client.Status().Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	p.abortWait = abortPreparationWait
	if err = p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	assertFencedLatePod(t, p, kube, l, pod)
}

// The fence for a Pod that never ran needs all three facts: it is being
// deleted, it has no node, and no container of any kind ever started.
func TestNeverScheduledNeedsDeletionNoNodeAndNoContainer(t *testing.T) {
	t.Parallel()
	now := metav1.Now()
	unscheduled := func() *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now}, Status: corev1.PodStatus{Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{Name: nativeContainer}, {Name: runtimekube.CaptureContainer}}}}
	}
	for _, test := range []struct {
		name   string
		mutate func(*corev1.Pod)
		want   bool
	}{
		{"unscheduled and deleting", func(*corev1.Pod) {}, true},
		{"not deleting", func(pod *corev1.Pod) { pod.DeletionTimestamp = nil }, false},
		{"scheduled, no container yet", func(pod *corev1.Pod) { pod.Spec.NodeName = fixtureNode }, false},
		{"container started", func(pod *corev1.Pod) { pod.Status.ContainerStatuses[0].ContainerID = "containerd://native" }, false},
		{"init container started", func(pod *corev1.Pod) {
			pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", ContainerID: "containerd://init"}}
		}, false},
		{"ephemeral container started", func(pod *corev1.Pod) {
			pod.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{Name: "debug", ContainerID: "containerd://debug"}}
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			pod := unscheduled()
			test.mutate(pod)
			if got := sandboxpod.Observe(pod).NeverScheduled; got != test.want {
				t.Fatalf("neverScheduled = %v, want %v", got, test.want)
			}
		})
	}
}

// A late Pod that a node already took, or that started any container, may
// run: it stays unproven until its agent container is seen to stop.
func TestAbortPreparationKeepsScheduledLatePodUnproven(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*corev1.Pod)
	}{
		{"scheduled, no container ID yet", func(pod *corev1.Pod) {
			pod.Spec.NodeName = fixtureNode
			pod.Status = corev1.PodStatus{Phase: corev1.PodPending}
		}},
		{"init container started", func(pod *corev1.Pod) {
			pod.Status = corev1.PodStatus{Phase: corev1.PodPending,
				InitContainerStatuses: []corev1.ContainerStatus{{Name: "init", ContainerID: "containerd://init"}}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p, kube, l, _ := queuedPreparation(t)
			kube.deferTermination = true
			p.abortWait = 300 * time.Millisecond
			if _, err := createSandboxWith(t.Context(), kube, agentRun(t, kube, l), test.mutate); err != nil {
				t.Fatal(err)
			}
			if err := p.AbortPreparation(t.Context(), l); !errors.Is(err, errWriterFenceUnknown) {
				t.Fatalf("possibly running late Pod: %v", err)
			}
			local, err := p.load(l)
			if err != nil || local.WriterFenced {
				t.Fatalf("possibly running late Pod was fenced: %+v %v", local, err)
			}
			if pod, found := sandboxPod(t, kube, registration(t, p, l).RunUID); !found ||
				!slices.Contains(pod.Finalizers, runtimekube.ManagedWriterFenceFinalizer) {
				t.Fatal("possibly running late Pod lost its writer fence")
			}
		})
	}
}

// With the controller down, one abort gives up within its bound and names
// what is unproven, rather than outliving the caller's request.
func TestAbortPreparationIsBoundedWhileControllerIsDown(t *testing.T) {
	t.Parallel()
	p, _, l, _ := queuedPreparation(t)
	p.abortWait = 300 * time.Millisecond
	started := time.Now()
	err := p.AbortPreparation(t.Context(), l)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "the controller has not ended it") {
		t.Fatalf("abort with the controller down: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("abort outlived its bound: %v", elapsed)
	}
}

// What a home sees when the executor cannot prove an abort within its bound:
// the executor's own unavailable answer, before the home's request times
// out, not a transport failure.
func TestAbortPreparationAnswersUnavailableBeforeTheHomeTimesOut(t *testing.T) {
	t.Parallel()
	p, _, l, _ := queuedPreparation(t)
	p.abortWait = 300 * time.Millisecond
	c := serveProfile(t, p, l, 5*time.Second)
	err := c.AbortPreparation(t.Context(), l)
	var answer *profile.RemoteError
	if !errors.As(err, &answer) || answer.Code != profile.CodeUnavailable || answer.Status != http.StatusServiceUnavailable || answer.Transport != nil {
		t.Fatalf("home saw %v", err)
	}
	if err.Error() != "session profile: unavailable (HTTP 503)" {
		t.Fatalf("home's problem text: %q", err.Error())
	}
}

// A visit that may have had gateway authority is not a preparation: success
// would report no usage, so the abort refuses it and leaves it as it is.
func TestAbortPreparationRefusesVisitThatMayHaveSpent(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		setup func(*testing.T, *Placement, session.Launch, session.Incarnation)
		phase registry.Phase
	}{
		{"active", func(*testing.T, *Placement, session.Launch, session.Incarnation) {}, registry.Active},
		{"stopped", func(t *testing.T, p *Placement, l session.Launch, i session.Incarnation) {
			if _, err := p.Stop(t.Context(), l, i); err != nil {
				t.Fatal(err)
			}
		}, registry.Stopped},
		{"cancelled without sealed usage", func(t *testing.T, p *Placement, l session.Launch, _ session.Incarnation) {
			if _, err := p.registry.Cancel(t.Context(), registration(t, p, l).Reference); err != nil {
				t.Fatal(err)
			}
		}, registry.Cancelled},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p, _, _, l, data := setup(t)
			i, err := p.Prepare(t.Context(), l, data)
			if err != nil {
				t.Fatal(err)
			}
			if err = p.Activate(t.Context(), l, i); err != nil {
				t.Fatal(err)
			}
			test.setup(t, p, l, i)
			err = p.AbortPreparation(t.Context(), l)
			if !isConflict(err) || !strings.Contains(err.Error(), "not a preparation") {
				t.Fatalf("abort of a visit that may have spent: %v", err)
			}
			local, err := p.load(l)
			if r := registration(t, p, l); err != nil || r.Phase != test.phase || local.PreparationAborted {
				t.Fatalf("refused abort changed the visit: %+v %+v %v", r, local, err)
			}
		})
	}
}

// An active visit that Abort cancelled carries a receipt with its usage.
// Accepting it here would let the coordinator record the cancellation without
// that usage, so it is a conflict.
func TestAbortPreparationRefusesCancelledVisitWithUsage(t *testing.T) {
	t.Parallel()
	p, _, _, l, data := setup(t)
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	spend(t, p, l)
	if o, err := p.Abort(t.Context(), l, i); err != nil || o.Stopped == nil || o.Stopped.Receipt == nil {
		t.Fatalf("abort of the active visit: %+v %v", o, err)
	}
	err = p.AbortPreparation(t.Context(), l)
	if !isConflict(err) || !strings.Contains(err.Error(), "not a preparation") {
		t.Fatalf("abort of a cancelled visit with usage: %v", err)
	}
}

// spend charges one unit and one micro to the visit's model account.
func spend(t *testing.T, p *Placement, l session.Launch) {
	t.Helper()
	r := registration(t, p, l)
	reservation, err := p.ledger.Reserve(t.Context(), account(r, api.CapabilityModelInference),
		budget.UsageReservation{RequireExisting: true, MaxUnits: 100, MaxCostMicros: 1000, Units: 1, CostMicros: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.ledger.Settle(t.Context(), reservation, budget.TrustedUsage{Units: 1, CostMicros: 1}); err != nil {
		t.Fatal(err)
	}
}

// An abort that wrote its tombstone but ran out of time before cancelling
// the registration leaves a prepared visit. Nothing may activate it after
// that, or a replay of the abort would find it cancelled by a later Abort
// and drop the usage it ran up.
func TestTombstonedPreparationCannotActivate(t *testing.T) {
	t.Parallel()
	p, _, _, l, data := setup(t)
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	local, err := p.load(l)
	if err != nil {
		t.Fatal(err)
	}
	local.PreparationAborted = true
	if err = p.save(local, nil); err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(t.Context(), l, i); !isConflict(err) {
		t.Fatalf("tombstoned preparation activated: %v", err)
	}
	if r := registration(t, p, l); r.Phase != registry.Prepared {
		t.Fatalf("refused activation changed the registration: %+v", r)
	}
	if err = p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatalf("replayed abort: %v", err)
	}
	if r := registration(t, p, l); r.Phase != registry.Cancelled || !unspent(r.Receipt) {
		t.Fatalf("replayed abort: %+v", r)
	}
}

// A controller status write between the executor's read of the run and its
// patch conflicts; the abort reads the run again rather than give up.
func TestAbortPreparationRetriesConflictingRunPatch(t *testing.T) {
	t.Parallel()
	p, kube, l, _ := queuedPreparation(t)
	kube.patchErr = apierrors.NewConflict(schema.GroupResource{Group: "sproozi.com", Resource: "agentruns"}, runName(l), errors.New("object was modified"))
	kube.reconcile = cancelReconcile(t, kube)
	if err := p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	if run := agentRun(t, kube, l); !run.Spec.Cancel || run.Status.Phase != api.AgentRunPhaseCancelled {
		t.Fatalf("run after a conflicting patch: %+v %+v", run.Spec, run.Status)
	}
}

// A registration that is gone was deleted by an administrator, and its run
// and Pod may remain: the abort is unproven whether the executor kept the
// registration's reference or finds it by run name. Found by name, no
// registration and no run means nothing was ever created.
func TestAbortPreparationOfDeletedRegistrationIsUnproven(t *testing.T) {
	t.Parallel()
	for _, keepReference := range []bool{true, false} {
		t.Run(fmt.Sprintf("reference kept %v", keepReference), func(t *testing.T) {
			t.Parallel()
			p, kube, l, _ := queuedPreparation(t)
			local, err := p.load(l)
			if err != nil {
				t.Fatal(err)
			}
			if !keepReference {
				local.Reference = registry.Reference{}
				if err = p.save(local, nil); err != nil {
					t.Fatal(err)
				}
			}
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: fixtureNamespace, Name: registry.NameForRun(runName(l))}}
			if err = kube.Client.Delete(t.Context(), cm); err != nil {
				t.Fatal(err)
			}
			err = p.AbortPreparation(t.Context(), l)
			if !isConflict(err) || !strings.Contains(err.Error(), "was deleted") {
				t.Fatalf("abort of a deleted registration: %v", err)
			}
		})
	}
	t.Run("never created", func(t *testing.T) {
		t.Parallel()
		p, _, _, l, data := setup(t)
		if _, err := p.persistIntent(l, data); err != nil {
			t.Fatal(err)
		}
		if err := p.AbortPreparation(t.Context(), l); err != nil {
			t.Fatalf("abort before any registration: %v", err)
		}
	})
}

// A run recreated at the registration's intended name is not the run it
// bound: the abort is unproven and says what an administrator should check.
func TestAbortPreparationOfReplacedRunNamesWhatToCheck(t *testing.T) {
	t.Parallel()
	p, kube, l, _ := queuedPreparation(t)
	run := agentRun(t, kube, l)
	if err := kube.Client.Delete(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	replacement := &api.AgentRun{ObjectMeta: metav1.ObjectMeta{Namespace: fixtureNamespace, Name: run.Name}, Spec: run.Spec}
	if err := kube.Create(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}
	err := p.AbortPreparation(t.Context(), l)
	if !isConflict(err) || !strings.Contains(err.Error(), "an administrator replaced it") || !strings.Contains(err.Error(), string(run.UID)) {
		t.Fatalf("abort of a replaced run: %v", err)
	}
}

// A launch the executor never saw claims its session epoch, so another
// allocation cannot prepare that epoch afterwards.
func TestAbortPreparationOfUnseenLaunchClaimsItsEpoch(t *testing.T) {
	t.Parallel()
	p, kube, _, l, data := setup(t)
	if err := p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	other := l
	other.AllocationID, other.OperationID = uuid.NewString(), uuid.NewString()
	_, err := p.Prepare(t.Context(), other, data)
	if !isConflict(err) || kube.runCreates != 0 {
		t.Fatalf("another allocation prepared the aborted epoch: %v, runs %d", err, kube.runCreates)
	}
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
	if err = p.save(local, nil); err != nil {
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

// A crash after the run was created but before it was bound to the
// registration leaves a run no registration names. Cancelling the
// registration keeps it unbound for good, so the controller never admits that
// run, and ends it as cancelled by the session.
func TestAbortPreparationOfRunNeverBound(t *testing.T) {
	t.Parallel()
	p, kube, _, l, data := setup(t)
	kube.queued = true
	ctx := t.Context()
	local, err := p.persistIntent(l, data)
	if err != nil {
		t.Fatal(err)
	}
	ceilings, err := p.liveProfile(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.registry.Create(ctx, p.registration(l, ceilings))
	if err != nil {
		t.Fatal(err)
	}
	local.Reference = r.Reference
	if err = p.save(local, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = p.ensureRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err = p.AbortPreparation(ctx, l); err != nil {
		t.Fatal(err)
	}
	r = registration(t, p, l)
	run := agentRun(t, kube, l)
	if r.Phase != registry.Cancelled || r.RunUID != "" || !run.Spec.Cancel {
		t.Fatalf("unbound run after abort: %+v %+v", r, run.Spec)
	}
	var template api.AgentTemplate
	if err = kube.Get(ctx, client.ObjectKey{Namespace: fixtureNamespace, Name: fixtureTemplate}, &template); err != nil {
		t.Fatal(err)
	}
	if _, err = registry.ResolveRun(ctx, kube, run, &template); !errors.Is(err, registry.ErrSessionCancelled) {
		t.Fatalf("controller would resolve the unbound run as %v", err)
	}
	assertRefusesPreparation(t, p, kube, l, data)
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
