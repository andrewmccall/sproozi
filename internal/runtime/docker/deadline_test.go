package docker

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/testutil"
	"github.com/google/uuid"
)

// A first visit whose activation failed, and whose prepared supervisor then
// stopped itself at the deadline, is ended by the coordinator through this
// placement: Inspect fails once the agent is gone, Abort fences and seals the
// prepared incarnation, and the execution is idle with its checkpoint kept
// and the sealed usage deducted once (ADR 0060).
func TestCoordinatorEndsAnExpiredPreparedDockerVisit(t *testing.T) {
	t.Parallel()
	data := fixtureArtifact(t)
	f := newFakeDocker()
	f.capture = data
	a := &fakeAuthority{engine: f}
	p := openFixture(t, testutil.PrivateDir(t), f, a)
	coordinatorDir := testutil.PrivateDir(t)
	if err := os.Chmod(coordinatorDir, 0700); err != nil {
		t.Fatal(err)
	}
	c, err := session.Open(coordinatorDir, map[string]session.Placement{localVolumeDriver: p}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	l := fixtureLaunch(t, data)
	l.Deadline = time.Now().UTC().Add(3 * time.Second)
	if _, err = c.Create(l, data, ""); err != nil {
		t.Fatal(err)
	}
	a.activateErr = errors.New("current allocation admission denied")
	pending, err := c.Recover(t.Context(), l.ExecutionID)
	if err == nil || pending.Phase != session.PhaseActivating {
		t.Fatalf("refused activation: %s %v", pending.Phase, err)
	}
	a.activateErr = nil
	agent := pending.Operations[0].Incarnation.ResourceUID
	time.Sleep(time.Until(l.Deadline))
	// The prepared supervisor stops itself at its deadline.
	f.containers[agent].State.Running, f.containers[agent].State.Pid = false, 0
	f.containers[agent].State.Status = exitedStatus
	if _, err = p.Inspect(t.Context(), l); err == nil {
		t.Fatal("Inspect of a prepared visit whose supervisor stopped succeeded")
	}
	r, err := c.Recover(t.Context(), l.ExecutionID)
	if !errors.Is(err, session.ErrDeadlinePassed) || r.Phase != session.PhaseIdle || len(r.Visits) != 1 ||
		r.Visits[0].Receipt == nil || r.Checkpoint.Metadata.SHA256 != l.CheckpointDigest || !a.revoked {
		t.Fatalf("expired prepared visit: %s %d visits %v", r.Phase, len(r.Visits), err)
	}
	if units := r.Remaining[modelFixtureCapability].Units.Value; units != 100-17 {
		t.Fatalf("remaining units %d, want %d", units, 100-17)
	}
	for _, container := range f.containers {
		if container.State.Running {
			t.Fatal("a container of the ended visit still runs")
		}
	}
	if again, err := c.Finalize(t.Context(), l.ExecutionID, uuid.NewString()); err != nil || again.Phase != session.PhaseIdle ||
		again.Remaining[modelFixtureCapability].Units.Value != 100-17 {
		t.Fatalf("stop after the end: %s %v", again.Phase, err)
	}
}

// The host stops an agent still running past its deadline, as after the host
// slept and the supervisor's timer ran late; the stop records a deadline exit.
func TestExitedStopsAnAgentPastItsDeadline(t *testing.T) {
	t.Parallel()
	data := fixtureArtifact(t)
	f := newFakeDocker()
	f.capture = data
	p := openFixture(t, testutil.PrivateDir(t), f, &fakeAuthority{engine: f})
	l := fixtureLaunch(t, data)
	l.Deadline = time.Now().UTC().Add(3 * time.Second)
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	if exited, err := p.Exited(t.Context(), l, i); err != nil || exited {
		t.Fatalf("exited before the deadline: %v %v", exited, err)
	}
	time.Sleep(time.Until(l.Deadline))
	if exited, err := p.Exited(t.Context(), l, i); err != nil || !exited {
		t.Fatalf("running past the deadline: exited %v %v", exited, err)
	}
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil || stopped.Exit == nil || !stopped.Exit.Deadline {
		t.Fatalf("deadline stop: %+v %v", stopped.Exit, err)
	}
}
