package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// A turnless stop whose kill is not confirmed by the deadline ends as the
// deadline, not as a failed stop.
func TestTurnlessKillUnconfirmedAtTheDeadlineIsReportedAsDeadline(t *testing.T) {
	if codexJournalExists(nativeHome) {
		t.Skip("a real Codex home is present")
	}
	setTurnlessSettle(t, 0)
	setQuitWait(t, time.Hour)
	neverConfirmed(t)
	s, _ := stopping(t, harness.NativeCodex, ignoresQuit, time.Now().Add(700*time.Millisecond))
	s.config.ThreadID, s.threadID = "", ""
	r, reports := stopOnce(t, s)
	failedOnce(t, r, reports, harness.NativeExitDeadline)
}

// Claude Code's lifecycle hooks are accepted, and status looks for the
// conversation, while the harness runs and while a graceful stop finishes
// it; not once its end is forced or settled.
func TestHooksAndStatusFollowTheHarnessUntilItsEndIsForced(t *testing.T) {
	withAdapter(t, harness.NativeCodex, func(a *nativeAdapter) {
		a.observe = func(*server) (string, string, error) { return selected, completedTurnID, nil }
	})
	path, err := harness.ClaudeProjectPath(workspace, selected)
	if err != nil {
		t.Fatal(err)
	}
	for p, observed := range map[phase]bool{phaseActive: true, phaseGated: true, phaseFinishing: true,
		phaseForcing: false, phaseEnded: false} {
		claude := &server{config: config{Harness: harness.NativeClaude, ThreadID: selected},
			life: lifecycle{phase: p}}
		r := claude.claudeHook(&claudeHook{SessionID: selected, TranscriptPath: filepath.Join(claudeHome, path),
			Cwd: workspace, Event: claudePromptSubmit, Prompt: "continue"})
		if (r.Error == "") != observed {
			t.Errorf("hook in %s: %+v", p, r)
		}
		codex := &server{config: config{Harness: harness.NativeCodex}, life: lifecycle{phase: p}}
		if got := codex.status().ThreadID; (got == selected) != observed {
			t.Errorf("status in %s found thread %q", p, got)
		}
	}
}

// A gated first visit counts as turnless while input is gated for a stop,
// gated or finishing, and not once the stop is forced.
func TestTurnlessOnlyWhileStopping(t *testing.T) {
	setTurnlessSettle(t, 0)
	withAdapter(t, harness.NativeCodex, func(a *nativeAdapter) {
		a.turnless = func(*server) bool { return true }
	})
	for p, want := range map[phase]bool{phaseActive: false, phaseGated: true, phaseFinishing: true,
		phaseForcing: false} {
		s := &server{config: config{Harness: harness.NativeCodex},
			life: lifecycle{phase: p, gatedAt: time.Now().Add(-time.Minute)}}
		if s.isTurnless() != want {
			t.Errorf("turnless in %s = %v", p, !want)
		}
		if p.stopping() && !s.status().Turnless {
			t.Errorf("status in %s does not report a turnless visit", p)
		}
	}
}

// The turnless latch is a fact, recorded whatever the phase it is found in,
// even one a forced stop reached meanwhile; the phase does not change.
func TestTurnlessLatchesInEveryStoppingPhase(t *testing.T) {
	for _, p := range []phase{phaseGated, phaseFinishing, phaseForcing, phaseEnded} {
		l := lifecycle{phase: p}
		if !l.apply(change{event: evTurnless}) || !l.turnless || l.phase != p {
			t.Errorf("turnless in %s: %+v", p, l)
		}
	}
}

// Input is gated once: a repeated gate keeps when it was first gated, which
// the turnless and input-settle waits run from.
func TestRepeatedGateKeepsWhenInputWasFirstGated(t *testing.T) {
	first := time.Unix(100, 0)
	l := lifecycle{phase: phaseActive}
	for _, at := range []time.Time{first, first.Add(time.Minute)} {
		if !l.apply(change{event: evGate, at: at}) {
			t.Fatalf("gate in %s refused", l.phase)
		}
	}
	if !l.gatedAt.Equal(first) {
		t.Fatalf("gated at %s, first gated at %s", l.gatedAt, first)
	}
}

// Copilot's observer waits for input to settle while the harness runs, but
// not once its end is forced or settled: an exited process takes no input.
func TestCopilotObservesWithoutWaitingOnceItsEndIsForced(t *testing.T) {
	for p, waits := range map[phase]bool{phaseGated: true, phaseForcing: false, phaseEnded: false} {
		s := &server{config: config{Harness: harness.NativeCopilot, ThreadID: selected, RolloutMaxBytes: 8192},
			life: lifecycle{phase: p, gatedAt: time.Now()}, inputGeneration: 1, lastInputAt: time.Now()}
		_, _, err := s.observeCopilotTurn()
		if errors.Is(err, harness.ErrCopilotTurnIncomplete) != waits {
			t.Errorf("copilot observation in %s: %v", p, err)
		}
	}
}
