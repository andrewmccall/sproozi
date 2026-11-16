package placement

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A fixture container that ends after a lost reply has ended before the
// reply is lost, so the tests need no long wait for it.
func init() { lostReplyWait = 300 * time.Millisecond }

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

// A resumed visit whose harness exited and whose captured state does not
// validate keeps the checkpoint it started from, once and for good: the
// state is fenced, so a retry would find the same. The discard is logged.
func TestExitedResumedVisitWhoseStateIsInvalidKeepsItsInput(t *testing.T) {
	t.Parallel()
	p, kube, e, l, data := setup(t)
	var log bytes.Buffer
	p.config.Log = &log
	i := activeVisit(t, p, l, data)
	endNative(t, p, kube, l, 1)
	e.logs = exitedReport(1, "")
	e.captureInvalid = true
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil {
		t.Fatalf("an invalid captured state after an exit failed the stop: %v", err)
	}
	if stopped.Exit == nil || stopped.ThreadID != fixtureThread || stopped.CheckpointDigest != l.CheckpointDigest ||
		stopped.Receipt == nil || e.captures != 1 {
		t.Fatalf("an exited visit whose state is invalid did not keep its input: thread %q, digest %q, exit %+v, %d captures",
			stopped.ThreadID, stopped.CheckpointDigest, stopped.Exit, e.captures)
	}
	if !strings.Contains(log.String(), "Discarded the captured state of stopped visit "+l.AllocationID) ||
		!strings.Contains(log.String(), "kept the checkpoint it started from") {
		t.Fatalf("the discard was not logged: %q", log.String())
	}
	if again, err := p.Stop(t.Context(), l, i); err != nil || again.CheckpointDigest != l.CheckpointDigest || e.captures != 1 {
		t.Fatalf("stop replay: digest %q, %d captures: %v", again.CheckpointDigest, e.captures, err)
	}
}

// A first visit whose harness reported a conversation and exited, and whose
// captured state does not validate, ends with no new checkpoint: the session
// keeps the workspace it started from, as on Docker.
func TestExitedFirstVisitWhoseStateIsInvalidKeepsNoCheckpoint(t *testing.T) {
	t.Parallel()
	p, kube, e, l, data := setup(t)
	l = freshLaunch(t, e, l, data, "")
	i := activeVisit(t, p, l, e.initial)
	endNative(t, p, kube, l, 0)
	e.logs = exitedReport(0, fixtureThread)
	e.captureInvalid = true
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil {
		t.Fatalf("an invalid captured state after an exit failed the stop: %v", err)
	}
	if stopped.Exit == nil || stopped.CheckpointDigest != "" || len(stopped.Checkpoint) != 0 || stopped.Receipt == nil {
		t.Fatalf("an exited first visit whose state is invalid: digest %q, exit %+v", stopped.CheckpointDigest, stopped.Exit)
	}
	if again, err := p.Stop(t.Context(), l, i); err != nil || again.CheckpointDigest != "" || again.Exit == nil {
		t.Fatalf("stop replay: digest %q: %v", again.CheckpointDigest, err)
	}
}

// A capture that fails for any reason other than the state, such as the
// helper failing, never keeps what the visit started from: the stop fails
// and its retry captures the conversation.
func TestExitedVisitWhoseHelperFailsCapturesOnRetry(t *testing.T) {
	t.Parallel()
	p, kube, e, l, data := setup(t)
	i := activeVisit(t, p, l, data)
	work, meta := cachedFixtureArtifact(t, harness.NativeCodex, "work of this visit\n")
	e.data, e.captureFailures = work, 1
	endNative(t, p, kube, l, 1)
	e.logs = exitedReport(1, "")
	if _, err := p.Stop(t.Context(), l, i); err == nil {
		t.Fatal("a failed capture helper kept what the visit started from")
	}
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil || stopped.CheckpointDigest != meta.SHA256 || stopped.Exit == nil {
		t.Fatalf("retry: digest %q, exit %+v: %v", stopped.CheckpointDigest, stopped.Exit, err)
	}
}

// A capture cut off by the stop's own context, at its bound or by an abort,
// never falls back, even when what came back would not validate: the stop
// fails and its retry decides again.
func TestExitedVisitWhoseCaptureRanOutOfTimeCapturesOnRetry(t *testing.T) {
	t.Parallel()
	p, kube, e, l, data := setup(t)
	i := activeVisit(t, p, l, data)
	work, meta := cachedFixtureArtifact(t, harness.NativeCodex, "work of this visit\n")
	e.data = work
	endNative(t, p, kube, l, 1)
	e.logs = exitedReport(1, "")
	ctx, cancel := context.WithCancel(t.Context())
	e.onCommand = func(command string) error {
		if command == helperCapture {
			cancel()
			e.captureInvalid = true
		}
		return nil
	}
	if _, err := p.Stop(ctx, l, i); err == nil {
		t.Fatal("a capture whose stop ended kept what the visit started from")
	}
	e.onCommand, e.captureInvalid = nil, false
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil || stopped.CheckpointDigest != meta.SHA256 {
		t.Fatalf("retry: digest %q: %v", stopped.CheckpointDigest, err)
	}
}

// A stop this placement asked for whose quiesce reply was lost, because the
// native container ended first, is a graceful stop of a harness whose state
// proves completion, as on Docker: no exit is recorded, and a capture that
// fails once is retried and keeps the conversation. The stop's own call
// decides when it sees the container end; otherwise its retry does, from the
// supervisor ending with no exit report.
func TestLostQuiesceReplyOfCodexIsAGracefulStop(t *testing.T) {
	t.Parallel()
	for _, retried := range []bool{false, true} {
		t.Run(map[bool]string{false: "in the stop", true: "on the retry"}[retried], func(t *testing.T) {
			t.Parallel()
			p, _, e, l, data := setup(t)
			i := activeVisit(t, p, l, data)
			work, meta := cachedFixtureArtifact(t, harness.NativeCodex, "work of this visit\n")
			e.data, e.lostQuiesce, e.captureFailures = work, true, 1
			ctx := t.Context()
			if retried {
				// The stop's call ends as the reply is lost.
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				e.onCommand = func(command string) error {
					if command == string(harness.NativeQuiesce) {
						cancel()
					}
					return nil
				}
				if _, err := p.Stop(ctx, l, i); err == nil {
					t.Fatal("a lost reply proved the stop in the call that lost it")
				}
				e.onCommand = nil
			}
			if _, err := p.Stop(t.Context(), l, i); err == nil {
				t.Fatal("a failed capture after a lost reply was not retried")
			}
			stopped, err := p.Stop(t.Context(), l, i)
			if err != nil || stopped.Exit != nil || stopped.CheckpointDigest != meta.SHA256 || stopped.ThreadID != fixtureThread {
				t.Fatalf("lost reply: digest %q, exit %+v: %v", stopped.CheckpointDigest, stopped.Exit, err)
			}
		})
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
