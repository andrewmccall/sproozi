package docker

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/testutil"
)

// The Docker stop decides what it keeps by the shared rule
// (session.StopKeeps, ADR 0062) with the outcome of the rule it had before:
// it captures exactly when the earlier needsCapture did, and only an exited
// harness's capture may keep what the visit started from instead of failing
// the stop. Which failures do is session.FallsBack's, below.
func TestNeedsCaptureKeepsTheEarlierDockerRule(t *testing.T) {
	t.Parallel()
	thread := "019a0000-0000-7000-8000-000000000001"
	for bits := range 64 {
		r := placementRecord{Turnless: bits&1 != 0}
		if bits&2 != 0 {
			r.Exited = &session.Exit{}
		}
		proves := bits&4 != 0
		if bits&8 != 0 {
			r.Thread = thread
		}
		if bits&16 != 0 {
			r.Launch.ThreadID, r.Launch.CheckpointDigest = thread, strings.Repeat("a", 64)
		}
		if bits&32 != 0 {
			r.CheckpointDigest = strings.Repeat("b", 64)
		}
		// The rule as it was before the shared one.
		exitedWithout := r.Exited != nil && (!proves || r.thread() == "")
		before := r.CheckpointDigest == "" && !r.Turnless && !exitedWithout
		if got := r.needsCapture(proves); got != before {
			t.Fatalf("record %d (proves %v): needsCapture %v, before %v", bits, proves, got, before)
		}
		// Before, a failed capture failed the stop exactly when nothing
		// exited; now only an exited one may fall back.
		if _, onFailure := r.kept(proves); before && (onFailure != 0) != (r.Exited != nil) {
			t.Fatalf("record %d: a failed capture falls back to %v", bits, onFailure)
		}
	}
}

// exitedDockerVisit is an active resumed Codex visit whose agent container
// exited by itself with a report.
func exitedDockerVisit(t *testing.T) (*Placement, *fakeDocker, session.Launch, session.Incarnation, []byte) {
	t.Helper()
	data := fixtureArtifact(t)
	f := newFakeDocker()
	f.capture = data
	p := openFixture(t, testutil.PrivateDir(t), f, &fakeAuthority{engine: f})
	l := fixtureLaunch(t, data)
	l.Deadline = time.Now().UTC().Add(time.Hour)
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	c := f.containers[i.ResourceUID]
	c.State.Running, c.State.Pid, c.State.Status = false, 0, exitedStatus
	c.logs = []byte(harness.NativeExitReportPrefix + `{"code":1,"cause":"exited"}` + "\n")
	return p, f, l, i, data
}

// A capture helper that fails after an exit fails the stop, and its retry
// captures the conversation. Before ADR 0062's correction, Docker kept the
// checkpoint the visit started from on any capture failure of an exited
// harness.
func TestExitedDockerVisitWhoseHelperFailsCapturesOnRetry(t *testing.T) {
	t.Parallel()
	p, f, l, i, data := exitedDockerVisit(t)
	f.failCapture = true
	if _, err := p.Stop(t.Context(), l, i); err == nil {
		t.Fatal("a failed capture helper kept what the visit started from")
	}
	f.failCapture = false
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil || !bytes.Equal(stopped.Checkpoint, data) || stopped.Exit == nil {
		t.Fatalf("retry: digest %q, exit %+v: %v", stopped.CheckpointDigest, stopped.Exit, err)
	}
}

// A captured state that does not validate after an exit keeps no new
// checkpoint, once, and the discard is logged.
func TestExitedDockerVisitWhoseStateIsInvalidKeepsWhatItStartedFrom(t *testing.T) {
	t.Parallel()
	p, f, l, i, _ := exitedDockerVisit(t)
	var log bytes.Buffer
	p.SetLog(&log)
	f.capture = []byte(`{"version":2,"harness":"codex"}`)
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil || stopped.CheckpointDigest != "" || len(stopped.Checkpoint) != 0 || stopped.Exit == nil {
		t.Fatalf("invalid state: digest %q, exit %+v: %v", stopped.CheckpointDigest, stopped.Exit, err)
	}
	if !strings.Contains(log.String(), "Discarded the captured state of stopped visit "+l.AllocationID) ||
		!strings.Contains(log.String(), "kept the checkpoint it started from") {
		t.Fatalf("the discard was not logged: %q", log.String())
	}
	if again, err := p.Stop(t.Context(), l, i); err != nil || again.CheckpointDigest != "" {
		t.Fatalf("stop replay: digest %q: %v", again.CheckpointDigest, err)
	}
}
