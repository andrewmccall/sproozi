package docker

import (
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/testutil"
)

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
