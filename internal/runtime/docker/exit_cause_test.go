package docker

import (
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/testutil"
)

// The visit keeps the cause the supervisor reported: only a harness that
// exited by itself with a nonzero code before its deadline is a failure, and
// a supervisor's deadline stop is a deadline whatever the host's clock says.
func TestExitedVisitKeepsTheReportedCause(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		report           string
		deadline, failed bool
		cause            harness.NativeExitCause
	}{
		{report: `{"code":2,"cause":"exited","output":"bad flag"}`, failed: true, cause: harness.NativeExitSelf},
		{report: `{"code":-1,"cause":"terminated"}`, cause: harness.NativeExitTerminated},
		{report: `{"code":-1,"cause":"failed"}`, cause: harness.NativeExitFailed},
		{report: `{"code":-1,"cause":"deadline"}`, deadline: true, cause: harness.NativeExitDeadline},
		// A report without a cause is ignored; the container's own code,
		// 0 in this fixture, stands.
		{report: `{"code":2}`},
	} {
		t.Run(string(tc.cause), func(t *testing.T) {
			t.Parallel()
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
			c.logs = []byte("starting\n" + harness.NativeExitReportPrefix + tc.report + "\n")
			stopped, err := p.Stop(t.Context(), l, i)
			if err != nil || stopped.Exit == nil {
				t.Fatalf("stop of an exited visit: %+v %v", stopped.Exit, err)
			}
			if e := stopped.Exit; e.Cause != tc.cause || e.Deadline != tc.deadline || e.Failed() != tc.failed {
				t.Fatalf("exit %+v failed=%v, want cause %q deadline %v failed %v",
					e, e.Failed(), tc.cause, tc.deadline, tc.failed)
			}
		})
	}
}
