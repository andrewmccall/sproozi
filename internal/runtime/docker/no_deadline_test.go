package docker

import (
	"slices"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/deadline"
	"github.com/andrewmccall/sproozi/internal/testutil"
)

// A visit without a deadline runs: its supervisor and helper hold until
// stopped, Exited does not stop it, and an agent that crashes is reported as
// an exit, not a deadline stop (ADR 0056).
func TestVisitWithoutDeadline(t *testing.T) {
	t.Parallel()
	data := fixtureArtifact(t)
	f := newFakeDocker()
	f.capture = data
	p := openFixture(t, testutil.PrivateDir(t), f, &fakeAuthority{engine: f})
	l := fixtureLaunch(t, data)
	l.Deadline = time.Time{}
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	agent := f.containers[i.ResourceUID]
	if !slices.Equal(agent.Config.Cmd[len(agent.Config.Cmd)-2:], []string{deadlineFlag, deadline.NoneArg}) {
		t.Fatalf("agent command = %v", agent.Config.Cmd)
	}
	if exited, err := p.Exited(t.Context(), l, i); err != nil || exited || !agent.State.Running {
		t.Fatalf("visit without deadline stopped: exited %v %v", exited, err)
	}
	agent.State.Running, agent.State.Pid, agent.State.Status = false, 0, exitedStatus
	if exited, err := p.Exited(t.Context(), l, i); err != nil || !exited {
		t.Fatalf("crashed agent not reported: %v %v", exited, err)
	}
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil || stopped.Exit == nil || stopped.Exit.Deadline {
		t.Fatalf("crash recorded as deadline stop: %+v %v", stopped.Exit, err)
	}
}
