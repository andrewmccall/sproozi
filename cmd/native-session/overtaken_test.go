package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
)

// overtaken is a supervisor of kind whose turn completed and whose harness
// force overtakes as the graceful stop starts to finish it.
func overtaken(t *testing.T, kind harness.NativeKind, force func(s *server)) (response, harness.NativeExitReport, *server) {
	t.Helper()
	var s *server
	withAdapter(t, kind, func(a *nativeAdapter) {
		a.observe = func(*server) (string, string, error) { return selected, completedTurnID, nil }
		a.turnless = func(*server) bool { return false }
		own := a.finish
		a.finish = func(n *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error) {
			force(s)
			return own(n, ctx)
		}
	})
	var reports *lockedBuffer
	var watched chan struct{}
	s, reports, watched = slowlyWatched(t, readyHarness(t))
	s.config.Harness = kind
	r := s.quiesce()
	return r, onlyReport(t, s, reports, watched), s
}

func terminated(s *server) { s.stop(evTerminate) }

func atTheDeadline(s *server) {
	deadlineIn(s, -time.Second)
	s.expire()
}

// A Codex stop that a forced stop overtook while Codex was asked to quit is
// not acknowledged: the quit never ended Codex. The reply names the forced
// cause, as the exit report does, and the supervisor exits nonzero.
func TestCodexStopOvertakenByAForcedStopIsNotAcknowledged(t *testing.T) {
	for name, c := range map[string]struct {
		force func(s *server)
		cause harness.NativeExitCause
	}{
		"terminated":      {terminated, harness.NativeExitTerminated},
		"at the deadline": {atTheDeadline, harness.NativeExitDeadline},
	} {
		t.Run(name, func(t *testing.T) {
			r, report, s := overtaken(t, harness.NativeCodex, c.force)
			if r.State != harness.NativeStopping || !strings.Contains(r.Error, string(c.cause)) ||
				report.Cause != c.cause || supervisorExitCode(s) == 0 {
				t.Fatalf("reply %+v, report %+v, exit code %d", r, report, supervisorExitCode(s))
			}
		})
	}
}

// Claude Code and Copilot end with a kill once their turn is observed
// complete, so a forced stop's kill that lands first ends them just as their
// own would: the stop is acknowledged with its conversation and turn, and
// the turn is kept. The exit report still names the forced cause.
func TestKilledHarnessStopOvertakenAfterItsTurnIsAcknowledged(t *testing.T) {
	for _, kind := range []harness.NativeKind{harness.NativeClaude, harness.NativeCopilot} {
		for name, c := range map[string]struct {
			force func(s *server)
			cause harness.NativeExitCause
		}{
			"terminated":      {terminated, harness.NativeExitTerminated},
			"at the deadline": {atTheDeadline, harness.NativeExitDeadline},
		} {
			t.Run(string(kind)+" "+name, func(t *testing.T) {
				r, report, _ := overtaken(t, kind, c.force)
				if r.State != harness.NativeStopped || r.Error != "" || r.ThreadID != selected ||
					r.TurnID != completedTurnID || report.Cause != c.cause {
					t.Fatalf("reply %+v, report %+v", r, report)
				}
			})
		}
	}
}

// A turnless stop that a forced stop overtook is not acknowledged either:
// the reply names the cause the report does.
func TestTurnlessStopOvertakenByAForcedStopIsNotAcknowledged(t *testing.T) {
	setTurnlessSettle(t, 0)
	withAdapter(t, harness.NativeCodex, func(a *nativeAdapter) {
		a.turnless = func(*server) bool { return true }
	})
	n := readyHarness(t)
	s, reports, watched := slowlyWatched(t, n)
	s.config.ThreadID, s.threadID = "", ""
	previous := forceStop
	forceStop = func(n *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error) {
		terminated(s)
		return previous(n, ctx)
	}
	t.Cleanup(func() { forceStop = previous })
	r := s.quiesce()
	report := onlyReport(t, s, reports, watched)
	if r.State != harness.NativeStopping || r.Turnless || !strings.Contains(r.Error, "terminated") ||
		report.Cause != harness.NativeExitTerminated {
		t.Fatalf("reply %+v, report %+v", r, report)
	}
}
