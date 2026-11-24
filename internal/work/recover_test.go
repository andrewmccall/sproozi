package work

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session"
)

// A task whose first stop could not be proven, and whose retried stop finds
// the writer still waiting for the task's turn, goes back to its completion
// wait at once: the retry is no failure, so nothing is logged, no backoff is
// waited out, and an earlier problem is cleared.
func TestRecoveredStopStillWaitingForTheTurnStepsAgain(t *testing.T) {
	t.Parallel()
	e := newDriverEnv(t, false, clockNone)
	namedJournalState(t, string(session.PhaseUnknownStop)).build(t, e)
	r, err := e.o.read(e.id)
	if err != nil {
		t.Fatal(err)
	}
	r.Problem = "an earlier failure"
	if err = e.o.save(r); err != nil {
		t.Fatal(err)
	}
	e.fixture(func(p *fixturePlacement) { p.turnPending = 1 })
	if n := e.o.step(t.Context(), e.id); n != nextPaced {
		t.Fatalf("recovered stop waiting for the turn: %s, want paced", nextNames[n])
	}
	if r, err = e.o.read(e.id); err != nil {
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
		if !continues(e.o.step(t.Context(), e.id)) {
			break
		}
	}
	if v, err := e.o.Info(t.Context(), e.id); err != nil || v.Phase != PhaseRetained {
		t.Fatalf("task after its turn completed: %s %v", v.Phase, err)
	}
}

// A retried stop that finds the turn pending but cannot save that in the
// journal is a failure, not a turn still running: the journal still holds
// the stop as unknown, so the driver logs it and backs off rather than
// stepping again as if the stop were waiting.
func TestRecoveredStopWhoseJournalCannotBeSavedBacksOff(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the save is made to fail by removing the directory's write permission, which Windows does not enforce through Chmod")
	}
	t.Parallel()
	e := newDriverEnv(t, false, clockNone)
	namedJournalState(t, string(session.PhaseUnknownStop)).build(t, e)
	e.fixture(func(p *fixturePlacement) { p.turnPending = 1 })
	dir := journalDir(t, e)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	n := e.o.step(t.Context(), e.id)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
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
	if n != nextWait || rec.Phase != session.PhaseUnknownStop || !strings.Contains(logged, "Could not continue work session") {
		t.Fatalf("unsaved turn-pending stop: %s, journal %s, log %q", nextNames[n], rec.Phase, logged)
	}
}

// journalDir is the directory holding the session's execution journal.
func journalDir(t *testing.T, e *driverEnv) string {
	t.Helper()
	var dir string
	err := filepath.WalkDir(e.journals, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == e.id+".json" {
			dir = filepath.Dir(path)
		}
		return err
	})
	if err != nil || dir == "" {
		t.Fatalf("journal of %s not found: %v", e.id, err)
	}
	return dir
}
