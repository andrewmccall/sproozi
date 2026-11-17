package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
)

// A graceful stop that a forced stop overtook while the harness was
// finishing does not reply stopped: the supervisor reports the forced
// cause and exits nonzero, and the reply agrees, as an error that is not
// acknowledged. For Claude Code this is also the reply a kill at the
// deadline gave before the supervisor alone owned the deadline: the
// terminal's own kill landed first, with reason deadline, which Claude
// Code's settle refused.
func TestStopOvertakenByAForcedStopIsNotAcknowledged(t *testing.T) {
	for name, c := range map[string]struct {
		kind  harness.NativeKind
		force func(s *server)
		cause harness.NativeExitCause
	}{
		"codex terminated": {harness.NativeCodex, func(s *server) { s.stop(evTerminate) }, harness.NativeExitTerminated},
		"claude code at the deadline": {harness.NativeClaude, func(s *server) {
			deadlineIn(s, -time.Second)
			s.expire()
		}, harness.NativeExitDeadline},
	} {
		t.Run(name, func(t *testing.T) {
			var s *server
			withAdapter(t, c.kind, func(a *nativeAdapter) {
				a.observe = func(*server) (string, string, error) { return selected, completedTurnID, nil }
				a.turnless = func(*server) bool { return false }
				own := a.finish
				a.finish = func(n *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error) {
					c.force(s)
					return own(n, ctx)
				}
			})
			n := readyHarness(t)
			var reports *lockedBuffer
			var watched chan struct{}
			s, reports, watched = slowlyWatched(t, n)
			s.config.Harness = c.kind
			r := s.quiesce()
			report := onlyReport(t, s, reports, watched)
			if r.State != harness.NativeStopping || !strings.Contains(r.Error, string(c.cause)) ||
				report.Cause != c.cause || supervisorExitCode(s) == 0 {
				t.Fatalf("reply %+v, report %+v, exit code %d", r, report, supervisorExitCode(s))
			}
		})
	}
}
