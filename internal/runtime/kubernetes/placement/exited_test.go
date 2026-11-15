package placement

import (
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// endNative records that the visit's native container ended by itself with
// code, as the kubelet reports it.
func endNative(t *testing.T, p *Placement, kube client.Client, l session.Launch, code int32) {
	t.Helper()
	_, r, err := p.retained(t.Context(), l)
	if err != nil {
		t.Fatal(err)
	}
	pod, err := p.pod(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	pod.Status.ContainerStatuses[0].State = fixtureTermination(pod.Status.ContainerStatuses[0].ContainerID, code)
	if err = kube.Status().Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
}

func activeVisit(t *testing.T, p *Placement, l session.Launch, data []byte) session.Incarnation {
	t.Helper()
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	return i
}

// A harness that exits on its own leaves the visit active until its owner
// stops it. Exited reports it, and the stop reports the supervisor's exit.
func TestExitedReportsANativeContainerThatEndedOnItsOwn(t *testing.T) {
	t.Parallel()
	p, kube, e, l, data := setup(t)
	i := activeVisit(t, p, l, data)
	if exited, err := p.Exited(t.Context(), l, i); err != nil || exited {
		t.Fatalf("a running harness reported as exited: %v %v", exited, err)
	}
	endNative(t, p, kube, l, 2)
	e.logs = []byte("starting\n" + harness.NativeExitReportPrefix + `{"code":2,"cause":"exited","output":"model refused"}` + "\n")
	if exited, err := p.Exited(t.Context(), l, i); err != nil || !exited {
		t.Fatalf("an ended harness not reported: %v %v", exited, err)
	}
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Exit == nil || stopped.Exit.Code != 2 || stopped.Exit.Cause != harness.NativeExitSelf ||
		stopped.Exit.Output != "model refused" || !stopped.Exit.Failed() {
		t.Fatalf("exit not reported: %+v", stopped.Exit)
	}
	if stopped.ThreadID != fixtureThread || stopped.CheckpointDigest == "" || stopped.Receipt == nil {
		t.Fatalf("an exited visit with a conversation keeps its checkpoint: %+v", stopped)
	}
	if exited, err := p.Exited(t.Context(), l, i); err != nil || exited {
		t.Fatalf("a stopped visit reported as exited: %v %v", exited, err)
	}
	// A replayed stop reports the same exit.
	again, err := p.Stop(t.Context(), l, i)
	if err != nil || again.Exit == nil || *again.Exit != *stopped.Exit {
		t.Fatalf("stop replay lost the exit: %+v %v", again.Exit, err)
	}
}

// Without a report, the container's own exit code is the exit.
func TestExitedWithoutAReportUsesTheContainerExitCode(t *testing.T) {
	t.Parallel()
	p, kube, _, l, data := setup(t)
	i := activeVisit(t, p, l, data)
	endNative(t, p, kube, l, 3)
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Exit == nil || stopped.Exit.Code != 3 || stopped.Exit.Cause != "" || !stopped.Exit.Failed() {
		t.Fatalf("container exit not reported: %+v", stopped.Exit)
	}
}

// A first visit whose harness exited before any conversation stops turnless:
// nothing is captured and the session keeps the workspace it started from.
func TestExitedFirstVisitWithoutConversationStopsTurnless(t *testing.T) {
	t.Parallel()
	p, kube, e, l, data := setup(t)
	l.ThreadID, l.CheckpointDigest = "", ""
	e.initial = initialWorkspace(t, data)
	i := activeVisit(t, p, l, e.initial)
	endNative(t, p, kube, l, 0)
	e.logs = []byte(harness.NativeExitReportPrefix + `{"code":0,"cause":"exited"}` + "\n")
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.ThreadID != "" || stopped.CheckpointDigest != "" || stopped.Receipt == nil || e.captureArgs != nil {
		t.Fatalf("a turnless exit captured a conversation: %+v", stopped)
	}
	if stopped.Exit == nil || stopped.Exit.Failed() {
		t.Fatalf("a clean exit reported as failed: %+v", stopped.Exit)
	}
}

// A first Codex visit that reported a completed turn binds that thread, so
// the stop keeps its conversation.
func TestExitedFirstVisitBindsTheReportedThread(t *testing.T) {
	t.Parallel()
	p, kube, e, l, data := setup(t)
	l.ThreadID, l.CheckpointDigest = "", ""
	e.initial = initialWorkspace(t, data)
	i := activeVisit(t, p, l, e.initial)
	endNative(t, p, kube, l, 0)
	e.logs = []byte(harness.NativeExitReportPrefix + `{"code":0,"cause":"exited","thread":"` + fixtureThread + `"}` + "\n")
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.ThreadID != fixtureThread || stopped.CheckpointDigest == "" {
		t.Fatalf("the reported conversation was not kept: %+v", stopped)
	}
}
