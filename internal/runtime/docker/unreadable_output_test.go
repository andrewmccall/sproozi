package docker

import (
	"testing"

	"github.com/andrewmccall/sproozi/internal/testutil"
)

// A stop that finds the agent container stopped but cannot read its output
// cannot tell a graceful stop from an exit, so it records an exit.
func TestDockerStopThatCannotReadTheOutputRecordsAnExit(t *testing.T) {
	t.Parallel()
	data := fixtureArtifact(t)
	f := newFakeDocker()
	f.capture = data
	p := openFixture(t, testutil.PrivateDir(t), f, &fakeAuthority{engine: f})
	l := fixtureLaunch(t, data)
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	c := f.containers[i.ResourceUID]
	c.State.Running, c.State.Pid, c.State.Status = false, 0, exitedStatus
	f.failLogs = true
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil || stopped.Exit == nil {
		t.Fatalf("unreadable output: exit %+v: %v", stopped.Exit, err)
	}
}
