package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
)

func setForcedStopWait(t *testing.T, d time.Duration) {
	t.Helper()
	previous := forcedStopWait
	forcedStopWait = d
	t.Cleanup(func() { forcedStopWait = previous })
}

// neverConfirmed replaces the supervisor's kill with one whose exit is never
// confirmed, as for a process that survives it.
func neverConfirmed(t *testing.T) {
	t.Helper()
	previous := forceStop
	forceStop = func(_ *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error) {
		<-ctx.Done()
		return nativepty.Exit{}, ctx.Err()
	}
	t.Cleanup(func() { forceStop = previous })
}

// stopping is a watched supervisor of kind running script, with until as its
// deadline and its diagnostics kept.
func stopping(t *testing.T, kind harness.NativeKind, script string, until time.Time) (*server, *bytes.Buffer) {
	t.Helper()
	s := watched(startNative(t, script, until))
	s.config.Harness, s.config.Deadline, s.deadline = kind, until, until
	diagnostics := &bytes.Buffer{}
	s.diagnostics = diagnostics
	return s, diagnostics
}

// stopOnce quiesces s, concluding a stopped reply as the handler does, and
// returns the reply and the exit reports written.
func stopOnce(t *testing.T, s *server) (response, []harness.NativeExitReport) {
	t.Helper()
	var r response
	reports := ended(t, s, func() {
		r = quiesceWithin(t, s, 10*time.Second)
		if r.State == harness.NativeStopped {
			s.conclude()
		}
	})
	return r, reports
}

// failedOnce checks a stop that failed and reported cause once.
func failedOnce(t *testing.T, r response, reports []harness.NativeExitReport, cause harness.NativeExitCause) {
	t.Helper()
	if r.State != harness.NativeStopping || r.Error == "" {
		t.Fatalf("reply: %+v", r)
	}
	if len(reports) != 1 || reports[0].Cause != cause {
		t.Fatalf("reports: %+v, want one %q", reports, cause)
	}
}

// The forced stop stands only on the turn the stop observed: one that
// changed by the time the harness was killed fails the stop.
func TestChangedTurnAfterTheKillFailsTheStop(t *testing.T) {
	var calls atomic.Int32
	withAdapter(t, harness.NativeCodex, func(a *nativeAdapter) {
		a.observe = func(*server) (string, string, error) {
			return selected, fmt.Sprintf("turn-%d", calls.Add(1)), nil
		}
	})
	setQuitWait(t, 300*time.Millisecond)
	s, _ := stopping(t, harness.NativeCodex, ignoresQuit, time.Time{})
	r, reports := stopOnce(t, s)
	failedOnce(t, r, reports, harness.NativeExitFailed)
	if calls.Load() != 2 {
		t.Fatalf("turn observed %d times, want 2", calls.Load())
	}
}

// A kill not confirmed within the forced-stop wait fails the stop: the
// writer is not proven stopped.
func TestUnconfirmedKillFailsTheStop(t *testing.T) {
	completedCodexTurn(t)
	setQuitWait(t, 300*time.Millisecond)
	setForcedStopWait(t, 300*time.Millisecond)
	neverConfirmed(t)
	s, _ := stopping(t, harness.NativeCodex, ignoresQuit, time.Time{})
	r, reports := stopOnce(t, s)
	failedOnce(t, r, reports, harness.NativeExitFailed)
}

// A deadline that falls inside the forced-stop wait ends it as the deadline.
func TestDeadlineDuringTheForcedStopWaitIsReportedAsDeadline(t *testing.T) {
	completedCodexTurn(t)
	setQuitWait(t, 300*time.Millisecond)
	setForcedStopWait(t, time.Hour)
	neverConfirmed(t)
	s, _ := stopping(t, harness.NativeCodex, ignoresQuit, time.Now().Add(time.Second))
	r, reports := stopOnce(t, s)
	failedOnce(t, r, reports, harness.NativeExitDeadline)
}

// The turnless stop's wait for its kill has the quit bound.
func TestTurnlessStopIsBounded(t *testing.T) {
	if codexJournalExists(nativeHome) {
		t.Skip("a real Codex home is present")
	}
	setTurnlessSettle(t, 0)
	setQuitWait(t, 300*time.Millisecond)
	neverConfirmed(t)
	s, _ := stopping(t, harness.NativeCodex, ignoresQuit, time.Time{})
	s.config.ThreadID, s.threadID = "", ""
	r, reports := stopOnce(t, s)
	failedOnce(t, r, reports, harness.NativeExitFailed)
}

// Claude Code and Copilot end with a kill. Its wait has the quit bound even
// without a deadline, and a kill confirmed only after it still stops them,
// said as a late confirmation rather than an ignored quit.
func TestKillingHarnessesFinishWithinTheBound(t *testing.T) {
	for _, kind := range []harness.NativeKind{harness.NativeClaude, harness.NativeCopilot} {
		for name, late := range map[string]bool{"confirmed": false, "confirmed late": true} {
			t.Run(string(kind)+" "+name, func(t *testing.T) {
				setQuitWait(t, time.Second)
				var bound atomic.Bool
				withAdapter(t, kind, func(a *nativeAdapter) {
					a.observe = func(*server) (string, string, error) { return selected, completedTurnID, nil }
					finish := a.finish
					a.finish = func(n *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error) {
						until, ok := ctx.Deadline()
						bound.Store(ok && !until.After(time.Now().Add(quitWait)))
						exit, err := finish(n, ctx)
						if late {
							<-ctx.Done()
							return nativepty.Exit{}, ctx.Err()
						}
						return exit, err
					}
				})
				s, diagnostics := stopping(t, kind, ignoresQuit, time.Time{})
				r, reports := stopOnce(t, s)
				if !bound.Load() {
					t.Fatal("the finish had no bound")
				}
				if r.Error != "" || r.State != harness.NativeStopped || r.Exit == nil ||
					r.Exit.Reason != string(nativepty.ForcedStop) || len(reports) != 0 || s.exitCode != 0 {
					t.Fatalf("stop: %+v exit %+v reports %+v code %d", r, r.Exit, reports, s.exitCode)
				}
				said := diagnostics.String()
				if strings.Contains(said, "quit") || late != strings.Contains(said, "Confirmed the harness stopped") {
					t.Fatalf("diagnostics: %q", said)
				}
			})
		}
	}
}

// A harness that exits by itself as the bound passes was not killed: its
// own exit, a crash included, is reported as it was, never as a clean stop.
func TestExitAtTheBoundKeepsTheHarnessCode(t *testing.T) {
	withAdapter(t, harness.NativeCodex, func(a *nativeAdapter) {
		a.observe = func(*server) (string, string, error) { return selected, completedTurnID, nil }
		a.finish = func(n *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error) {
			<-ctx.Done()
			<-n.Done()
			return nativepty.Exit{}, ctx.Err()
		}
	})
	setQuitWait(t, 200*time.Millisecond)
	s, diagnostics := stopping(t, harness.NativeCodex, "sleep 1; exit 3", time.Time{})
	r, _ := stopOnce(t, s)
	if r.Exit == nil || r.Exit.Code != 3 || r.Exit.Reason != string(nativepty.NativeExit) || r.Error == "" {
		t.Fatalf("reply: %+v exit %+v", r, r.Exit)
	}
	if s.exitCode != 3 || strings.Contains(diagnostics.String(), "Forced") {
		t.Fatalf("settled with code %d, diagnostics %q", s.exitCode, diagnostics.String())
	}
}

// A harness whose full input buffer took no quit is not reading its terminal;
// with its turn complete and input gated, it is forced as at the bound.
func TestFullInputBufferTakesTheForcedPath(t *testing.T) {
	withAdapter(t, harness.NativeCodex, func(a *nativeAdapter) {
		a.observe = func(*server) (string, string, error) { return selected, completedTurnID, nil }
		a.finish = func(*nativepty.Supervisor, context.Context) (nativepty.Exit, error) {
			return nativepty.Exit{}, fmt.Errorf("send native quit: %w", errors.Join(nativepty.ErrInputBusy, io.ErrShortWrite))
		}
	})
	s, diagnostics := stopping(t, harness.NativeCodex, ignoresQuit, time.Time{})
	r, reports := stopOnce(t, s)
	if r.Error != "" || r.State != harness.NativeStopped || r.Exit == nil ||
		r.Exit.Reason != string(nativepty.ForcedStop) || len(reports) != 0 || s.exitCode != 0 {
		t.Fatalf("stop: %+v exit %+v reports %+v code %d", r, r.Exit, reports, s.exitCode)
	}
	if !strings.Contains(diagnostics.String(), "Forced the harness to stop") {
		t.Fatalf("diagnostics: %q", diagnostics.String())
	}
}
