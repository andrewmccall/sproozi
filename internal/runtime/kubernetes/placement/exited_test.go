package placement

import (
	"errors"
	"fmt"
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

// exitedReport is the supervisor's report of a harness that ended by itself,
// naming thread when it is not empty.
func exitedReport(code int, thread string) []byte {
	report := fmt.Sprintf(`{"code":%d,"cause":"exited"`, code)
	if thread != "" {
		report += `,"thread":"` + thread + `"`
	}
	return []byte(harness.NativeExitReportPrefix + report + "}\n")
}

// A resumed visit whose harness exited and whose capture then fails keeps the
// checkpoint it started from, as the Docker placement does, instead of
// failing the stop on every retry.
func TestExitedResumedVisitWhoseCaptureFailsKeepsItsInput(t *testing.T) {
	t.Parallel()
	p, kube, e, l, data := setup(t)
	i := activeVisit(t, p, l, data)
	endNative(t, p, kube, l, 1)
	e.logs = exitedReport(1, "")
	e.captureErr = errors.New("capture helper failed")
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil {
		t.Fatalf("a failed capture after an exit failed the stop: %v", err)
	}
	if stopped.Exit == nil || stopped.ThreadID != fixtureThread || stopped.CheckpointDigest != l.CheckpointDigest ||
		stopped.Receipt == nil || len(e.captureArgs) == 0 {
		t.Fatalf("an exited visit whose capture failed did not keep its input: thread %q, digest %q, exit %+v",
			stopped.ThreadID, stopped.CheckpointDigest, stopped.Exit)
	}
	if again, err := p.Stop(t.Context(), l, i); err != nil || again.CheckpointDigest != l.CheckpointDigest {
		t.Fatalf("stop replay: digest %q: %v", again.CheckpointDigest, err)
	}
}

// A first visit whose harness reported a conversation and exited, and whose
// capture then fails, ends with no new checkpoint: the session keeps the
// workspace it started from, as on Docker.
func TestExitedFirstVisitWhoseCaptureFailsKeepsNoCheckpoint(t *testing.T) {
	t.Parallel()
	p, kube, e, l, data := setup(t)
	l = freshLaunch(t, e, l, data, "")
	i := activeVisit(t, p, l, e.initial)
	endNative(t, p, kube, l, 0)
	e.logs = exitedReport(0, fixtureThread)
	e.captureErr = errors.New("capture helper failed")
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil {
		t.Fatalf("a failed capture after an exit failed the stop: %v", err)
	}
	if stopped.Exit == nil || stopped.CheckpointDigest != "" || len(stopped.Checkpoint) != 0 || stopped.Receipt == nil {
		t.Fatalf("an exited first visit whose capture failed: digest %q, exit %+v", stopped.CheckpointDigest, stopped.Exit)
	}
	if again, err := p.Stop(t.Context(), l, i); err != nil || again.CheckpointDigest != "" || again.Exit == nil {
		t.Fatalf("stop replay: digest %q: %v", again.CheckpointDigest, err)
	}
}

// A first Claude visit whose stop bound its conversation and answered that a
// turn is pending, and whose harness then exited by itself, keeps no
// conversation: a stopped container never proves a completed Claude turn
// (ADR 0011), so nothing is captured from it.
func TestExitedClaudeFirstVisitAfterTurnPendingCapturesNothing(t *testing.T) {
	t.Parallel()
	p, kube, e, l, data := setupKind(t, harness.NativeClaude)
	l = freshLaunch(t, e, l, data, "")
	i := activeVisit(t, p, l, e.initial)
	e.pendingQuiesce = true
	if _, err := p.Stop(t.Context(), l, i); !errors.Is(err, harness.ErrNativeTurnPending) {
		t.Fatalf("stop with a turn running: %v", err)
	}
	endNative(t, p, kube, l, 0)
	e.logs = exitedReport(0, "")
	if exited, err := p.Exited(t.Context(), l, i); err != nil || !exited {
		t.Fatalf("exit not noticed: %v %v", exited, err)
	}
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.captureArgs) != 0 || stopped.CheckpointDigest != "" || len(stopped.Checkpoint) != 0 ||
		stopped.Exit == nil || stopped.Receipt == nil {
		t.Fatalf("a conversation was claimed from a stopped Claude container: captured %v, digest %q, exit %+v",
			e.captureArgs, stopped.CheckpointDigest, stopped.Exit)
	}
	if again, err := p.Stop(t.Context(), l, i); err != nil || again.CheckpointDigest != "" || len(e.captureArgs) != 0 {
		t.Fatalf("stop replay: digest %q: %v", again.CheckpointDigest, err)
	}
}
