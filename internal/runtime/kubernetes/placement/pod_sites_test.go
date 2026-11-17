package placement

import (
	"context"
	"io"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	runtimekube "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/session"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Each test here pins one place the placement reads a sandbox Pod, through
// the operation that reads it.

// deleting marks a Pod read as being deleted.
func deleting(pod *corev1.Pod) {
	now := metav1.Now()
	pod.DeletionTimestamp = &now
}

// afterActiveCheck applies change, from now on, to every Pod read except
// registry.RequireActive's. An active visit's Pod is read by RequireActive
// and once more by the placement, so this models a deletion that lands
// between the two, however many reads there are. The returned counter is
// how many reads RequireActive made, which a test requires to be nonzero.
func afterActiveCheck(kube *admissionClient, change func(*corev1.Pod)) *atomic.Int64 {
	var checks atomic.Int64
	read := func(pod *corev1.Pod) {
		if readByRequireActive() {
			checks.Add(1)
			return
		}
		change(pod)
	}
	kube.podRead.Store(&read)
	return &checks
}

// readByRequireActive reports a Pod read made inside registry.RequireActive.
func readByRequireActive() bool {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		frame, more := frames.Next()
		if strings.HasSuffix(frame.Function, "/registry.RequireActive") {
			return true
		}
		if !more {
			return false
		}
	}
}

func requireActiveChecked(t *testing.T, checks *atomic.Int64) {
	t.Helper()
	if checks.Load() == 0 {
		t.Fatal("registry.RequireActive read no Pod: the test no longer models a deletion after it")
	}
}

func everyRead(kube *admissionClient, change func(*corev1.Pod)) {
	kube.podRead.Store(&change)
}

func TestActivateRefusesAPodBeingDeleted(t *testing.T) {
	t.Parallel()
	p, kube, _, l, data := setup(t)
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	everyRead(kube, deleting)
	if err = p.Activate(t.Context(), l, i); err == nil || !strings.Contains(err.Error(), "inert worker unavailable") {
		t.Fatalf("activated a Pod being deleted: %v", err)
	}
	if r := registration(t, p, l); r.Phase != registry.Prepared {
		t.Fatalf("refused activation recorded %s", r.Phase)
	}
}

func TestAttachRefusesAPodDeletedAfterItsActiveCheck(t *testing.T) {
	t.Parallel()
	p, kube, _, l, data := setup(t)
	i := activeVisit(t, p, l, data)
	checks := afterActiveCheck(kube, deleting)
	if err := p.Attach(t.Context(), l, i, strings.NewReader(""), io.Discard); !isConflict(err) {
		t.Fatalf("attached to a Pod being deleted: %v", err)
	}
	requireActiveChecked(t, checks)
}

// attachExecutor holds an attachment open until its context ends, and
// calls attached when it starts.
type attachExecutor struct {
	*protocolExecutor
	attached func()
}

func (e *attachExecutor) Execute(ctx context.Context, namespace, pod, container string, args []string, input io.Reader, output, errout io.Writer) error {
	if args[1] == string(harness.NativeAttach) {
		e.attached()
		<-ctx.Done()
		return ctx.Err()
	}
	return e.protocolExecutor.Execute(ctx, namespace, pod, container, args, input, output, errout)
}

func TestAttachEndsOnceThePodIsBeingDeleted(t *testing.T) {
	t.Parallel()
	p, kube, e, l, data := setup(t)
	i := activeVisit(t, p, l, data)
	var checks atomic.Pointer[atomic.Int64]
	p.exec = &attachExecutor{protocolExecutor: e, attached: func() { checks.Store(afterActiveCheck(kube, deleting)) }}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.Attach(ctx, l, i, strings.NewReader(""), io.Discard) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("attachment outlived its Pod's deletion")
	}
	requireActiveChecked(t, checks.Load())
}

func TestInspectOfAnActiveVisitWhosePodIsBeingDeleted(t *testing.T) {
	t.Parallel()
	p, kube, _, l, data := setup(t)
	activeVisit(t, p, l, data)
	checks := afterActiveCheck(kube, deleting)
	if o, err := p.Inspect(t.Context(), l); err != nil || o.State != session.PlacementUnknown {
		t.Fatalf("inspect of an active visit whose Pod is being deleted: %+v %v", o.State, err)
	}
	requireActiveChecked(t, checks)
}

// prepareWithin prepares l, giving the wait for a running Pod a short
// bound, and reports the registration's phase afterwards.
func prepareWithin(t *testing.T, p *Placement, l session.Launch, data []byte) (registry.Phase, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	_, err := p.Prepare(ctx, l, data)
	return registration(t, p, l).Phase, err
}

func TestPrepareWaitsForTheCaptureHelper(t *testing.T) {
	t.Parallel()
	p, kube, _, l, data := setup(t)
	everyRead(kube, func(pod *corev1.Pod) {
		for index := range pod.Status.ContainerStatuses {
			if pod.Status.ContainerStatuses[index].Name == runtimekube.CaptureContainer {
				pod.Status.ContainerStatuses[index].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
			}
		}
	})
	if phase, err := prepareWithin(t, p, l, data); err == nil || phase == registry.Prepared {
		t.Fatalf("prepared without a running capture helper: %s %v", phase, err)
	}
}

func TestPrepareDoesNotUseAPodBeingDeleted(t *testing.T) {
	t.Parallel()
	p, kube, _, l, data := setup(t)
	everyRead(kube, deleting)
	if phase, err := prepareWithin(t, p, l, data); err == nil || phase == registry.Prepared {
		t.Fatalf("prepared into a Pod being deleted: %s %v", phase, err)
	}
}

func TestStopNeedsTheCaptureHelperRunning(t *testing.T) {
	t.Parallel()
	p, kube, _, l, data := setup(t)
	i := activeVisit(t, p, l, data)
	everyRead(kube, func(pod *corev1.Pod) {
		pod.Status.ContainerStatuses[1].State = fixtureTermination(pod.Status.ContainerStatuses[1].ContainerID, 0)
	})
	_, err := p.Stop(t.Context(), l, i)
	if err == nil || !strings.Contains(err.Error(), "capture helper unavailable") {
		t.Fatalf("stopped without a capture helper: %v", err)
	}
	if r := registration(t, p, l); r.Receipt != nil {
		t.Fatalf("stop without a capture helper recorded a receipt: %+v", r.Receipt)
	}
}

func TestExitedIgnoresTheKubeletsPlaceholderTermination(t *testing.T) {
	t.Parallel()
	p, kube, _, l, data := setup(t)
	i := activeVisit(t, p, l, data)
	everyRead(kube, func(pod *corev1.Pod) {
		pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 137, Reason: containerStatusUnknown, Message: unknownTerminationMessage}}
	})
	if exited, err := p.Exited(t.Context(), l, i); err != nil || exited {
		t.Fatalf("placeholder termination reported as an exit: %v %v", exited, err)
	}
	everyRead(kube, func(pod *corev1.Pod) {
		pod.Status.ContainerStatuses[0].State = fixtureTermination(pod.Status.ContainerStatuses[0].ContainerID, 1)
	})
	if exited, err := p.Exited(t.Context(), l, i); err != nil || !exited {
		t.Fatalf("proven termination not reported as an exit: %v %v", exited, err)
	}
}
