package work

import (
	"testing"

	"github.com/andrewmccall/sproozi/internal/session"
)

// A task whose first stop could not be proven, and whose retried stop finds
// the writer still waiting for the task's turn, goes back to its completion
// wait at once: the retry is no failure, so nothing is logged and no
// backoff is waited out.
func TestRecoveredStopStillWaitingForTheTurnStepsAgain(t *testing.T) {
	t.Parallel()
	e := newDriverEnv(t, false, clockNone)
	namedJournalState(t, "unknown-stop").build(t, e)
	e.fixture(func(p *fixturePlacement) { p.turnPending = 1 })
	if n := e.o.step(t.Context(), e.id); n != nextAgain {
		t.Fatalf("recovered stop waiting for the turn: %s, want again", nextNames[n])
	}
	r, err := e.o.read(e.id)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := e.o.execution(r)
	if err != nil {
		t.Fatal(err)
	}
	e.logMu.Lock()
	logged := e.logs.String()
	e.logMu.Unlock()
	if rec.Phase != session.PhaseStopping || r.Problem != "" || logged != "" {
		t.Fatalf("recovered stop: phase %s, problem %q, log %q", rec.Phase, r.Problem, logged)
	}
	// The next step is the completion wait, which then finishes the task.
	for range driverStepLimit {
		if e.o.step(t.Context(), e.id) != nextAgain {
			break
		}
	}
	if v, err := e.o.Info(t.Context(), e.id); err != nil || v.Phase != PhaseRetained {
		t.Fatalf("task after its turn completed: %s %v", v.Phase, err)
	}
}
