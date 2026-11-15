package main

import (
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
	"golang.org/x/sys/unix"
)

// ignoresQuit is a harness that never reads its terminal, so it ignores the
// quit typed into it, as one showing a modal prompt or hung would.
const ignoresQuit = "while :; do sleep 1; done"

const completedTurnID = "turn-1"

// completedCodexTurn makes the Codex adapter observe a completed turn of
// selected while keeping its real quit, so a test drives the stop after it.
func completedCodexTurn(t *testing.T) {
	t.Helper()
	previous := nativeAdapters[harness.NativeCodex]
	adapter := previous
	adapter.observe = func(*server) (string, string, error) { return selected, completedTurnID, nil }
	nativeAdapters[harness.NativeCodex] = adapter
	t.Cleanup(func() { nativeAdapters[harness.NativeCodex] = previous })
}

func setQuitWait(t *testing.T, d time.Duration) {
	t.Helper()
	previous := quitWait
	quitWait = d
	t.Cleanup(func() { quitWait = previous })
}

// quiesceWithin runs quiesce and fails the test if it has not returned
// within limit, rather than hanging the suite.
func quiesceWithin(t *testing.T, s *server, limit time.Duration) response {
	t.Helper()
	replies := make(chan response, 1)
	go func() { replies <- s.quiesce() }()
	select {
	case r := <-replies:
		return r
	case <-time.After(limit):
		t.Fatalf("quiesce did not return within %s", limit)
	}
	return response{}
}

// A harness that ignores its quit after a completed turn is forced at the
// quit bound, with or without a deadline beyond it. The stop is graceful: the
// turn completed and input was gated, so the reply is stopped with the turn,
// names the forced stop, and no exit report calls it a deadline or the
// harness's own exit. A second stop does not wait behind the first.
func TestIgnoredQuitIsForcedAtTheQuitBound(t *testing.T) {
	for name, until := range map[string]time.Time{
		"no deadline":              {},
		"deadline after the bound": time.Now().Add(time.Hour),
	} {
		t.Run(name, func(t *testing.T) {
			completedCodexTurn(t)
			setQuitWait(t, 500*time.Millisecond)
			native := startNative(t, ignoresQuit, until)
			s := watched(native)
			s.config.Deadline, s.deadline = until, until
			second := make(chan response, 1)
			var r response
			reports := ended(t, s, func() {
				go func() {
					time.Sleep(100 * time.Millisecond)
					second <- s.quiesce()
				}()
				r = quiesceWithin(t, s, 10*time.Second)
				if r.State == harness.NativeStopped {
					s.conclude()
				}
			})
			if r.Error != "" || r.State != harness.NativeStopped || r.ThreadID != selected ||
				r.TurnID != completedTurnID || r.Exit == nil || r.Exit.Reason != string(nativepty.ForcedStop) {
				t.Fatalf("quiesce after an ignored quit: %+v exit %+v", r, r.Exit)
			}
			if len(reports) != 0 {
				t.Fatalf("a graceful stop wrote exit reports: %+v", reports)
			}
			if s.cause != causeQuiesced || s.exitCode == 0 {
				t.Fatalf("settled with cause %d and code %d", s.cause, s.exitCode)
			}
			if err := unix.Kill(-native.PID(), 0); err == nil {
				t.Fatal("the harness's process group is still running")
			}
			select {
			case again := <-second:
				if again.State != harness.NativeStopped {
					t.Fatalf("second quiesce: %+v", again)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("a second quiesce waited behind the first")
			}
			if st := s.status(); st.State != harness.NativeStopped || st.ThreadID != selected {
				t.Fatalf("status after the stop: %+v", st)
			}
		})
	}
}

// A deadline earlier than the quit bound still ends the stop, as the
// deadline: one exit report with that cause.
func TestDeadlineBeforeTheQuitBoundIsReportedAsDeadline(t *testing.T) {
	completedCodexTurn(t)
	setQuitWait(t, time.Hour)
	until := time.Now().Add(700 * time.Millisecond)
	native := startNative(t, ignoresQuit, until)
	s := watched(native)
	s.config.Deadline, s.deadline = until, until
	var r response
	reports := ended(t, s, func() { r = quiesceWithin(t, s, 10*time.Second) })
	if r.State != harness.NativeStopping || !strings.Contains(r.Error, "deadline") {
		t.Fatalf("quiesce at the deadline: %+v", r)
	}
	if len(reports) != 1 || reports[0].Cause != harness.NativeExitDeadline {
		t.Fatalf("deadline reports: %+v", reports)
	}
	if s.exitCode == 0 {
		t.Fatal("a stop at the deadline exited the supervisor cleanly")
	}
}
