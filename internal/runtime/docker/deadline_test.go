package docker

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/testutil"
	"github.com/google/uuid"
)

// unactivated is the Docker placement with Activate refused before it reaches
// Docker, so the record stays prepared while the coordinator records the
// activation as pending, as when the service stopped between the two.
type unactivated struct{ *Placement }

func (unactivated) Activate(context.Context, session.Launch, session.Incarnation) error {
	return errors.New("service stopped before activation")
}

// A first visit that did not activate before its deadline is ended by the
// coordinator through this placement, on either Docker route. If Docker's
// activation was refused, its record stays activating and Inspect re-runs the
// activation, which the deadline refuses. If activation never reached Docker,
// its record stays prepared, the supervisor stops itself at the deadline, and
// Inspect fails because the agent ended without a retained stop. Either way
// Abort fences and seals the prepared incarnation, and the execution is idle
// with its checkpoint kept and the sealed usage deducted once (ADR 0060).
func TestCoordinatorEndsAnExpiredPreparedDockerVisit(t *testing.T) {
	t.Parallel()
	for name, refusedByDocker := range map[string]bool{"activation refused": true, "activation never sent": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			endExpiredPreparedDockerVisit(t, refusedByDocker)
		})
	}
}

func endExpiredPreparedDockerVisit(t *testing.T, refusedByDocker bool) {
	t.Helper()
	data := fixtureArtifact(t)
	f := newFakeDocker()
	f.capture = data
	a := &fakeAuthority{engine: f}
	p := openFixture(t, testutil.PrivateDir(t), f, a)
	coordinatorDir := testutil.PrivateDir(t)
	if err := os.Chmod(coordinatorDir, 0700); err != nil {
		t.Fatal(err)
	}
	var placement session.Placement = p
	if !refusedByDocker {
		placement = unactivated{p}
	}
	l := fixtureLaunch(t, data)
	// The placement and the coordinator read one clock, which stands before
	// the deadline while the visit is set up and passes it on pastDeadline.
	clock, pastDeadline := deadlineClock(l)
	p.now = clock
	c, err := session.Open(coordinatorDir, map[string]session.Placement{localVolumeDriver: placement}, 1<<20,
		session.WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
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
	pastDeadline()
	// The prepared supervisor stops itself at its deadline.
	f.containers[agent].State.Running, f.containers[agent].State.Pid = false, 0
	f.containers[agent].State.Status = exitedStatus
	want := "docker native execution ended without retained stop"
	if refusedByDocker {
		want = "docker placement: activate refused after the launch deadline"
	}
	if _, err = p.Inspect(t.Context(), l); err == nil || err.Error() != want {
		t.Fatalf("Inspect of the expired visit: %v, want %q", err, want)
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
	clock, pastDeadline := deadlineClock(l)
	p.now = clock
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
	pastDeadline()
	if exited, err := p.Exited(t.Context(), l, i); err != nil || !exited {
		t.Fatalf("running past the deadline: exited %v %v", exited, err)
	}
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil || stopped.Exit == nil || !stopped.Exit.Deadline {
		t.Fatalf("deadline stop: %+v %v", stopped.Exit, err)
	}
}

// deadlineClock is a clock for launch l that reads an hour before its
// deadline, which fixtureLaunch sets, until pastDeadline moves it a second
// past the deadline. The test, not the machine's speed, decides when the
// deadline passes.
func deadlineClock(l session.Launch) (clock func() time.Time, pastDeadline func()) {
	var past atomic.Bool
	return func() time.Time {
		if past.Load() {
			return l.Deadline.Add(time.Second)
		}
		return l.Deadline.Add(-time.Hour)
	}, func() { past.Store(true) }
}
