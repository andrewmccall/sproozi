package work

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session"
)

// journalState finds the named state of journalStates.
func namedJournalState(t *testing.T, name string) journalState {
	t.Helper()
	for _, s := range journalStates() {
		if s.name == name {
			return s
		}
	}
	t.Fatalf("no journal state %s", name)
	return journalState{}
}

// driveRounds takes the driver's steps, without waiting out its backoff,
// for a few rounds, as the driver would over time.
func driveRounds(t *testing.T, e *driverEnv) {
	t.Helper()
	for range 4 {
		for range driverStepLimit {
			if e.o.step(t.Context(), e.id) != nextAgain {
				break
			}
		}
	}
}

// A task, or an interactive session, whose stop or cancellation is
// unsettled when its project is revoked is settled by the driver (ADR 0057,
// section 2): an interactive session's owner can no longer act on it, so
// "recover" cannot. A cancellation already held is carried out, never
// replaced by the project's; otherwise the session is cancelled for the
// revoked project. Either way the writer is fenced and the allocation
// released, and only sealed usage is deducted.
func TestRevokedUnsettledSessionIsSettled(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		state string
		held  bool
	}{
		{string(session.PhaseStopping), false},
		{string(session.PhaseUnknownStop), false},
		{string(session.PhaseUnknownUsage), false},
		{string(session.PhaseCancelling), true},
		{"cancel-unknown-stop", true},
		{"cancel-unknown-usage", true},
	} {
		for _, interactive := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s interactive=%t", tc.state, interactive), func(t *testing.T) {
				t.Parallel()
				e := newDriverEnv(t, interactive, clockNone)
				namedJournalState(t, tc.state).build(t, e)
				e.revoke()
				driveRounds(t, e)
				r, err := e.o.read(e.id)
				if err != nil {
					t.Fatal(err)
				}
				rec, err := e.o.execution(r)
				if err != nil {
					t.Fatal(err)
				}
				if rec.Phase != session.PhaseCancelled || rec.Active != nil || rec.PendingOperation != "" {
					t.Fatalf("revoked %s: phase %s, writer held %t, pending %q", tc.state, rec.Phase,
						rec.Active != nil, rec.PendingOperation)
				}
				// A cancellation already held wins over the revoked project.
				wantOp, wantProblem := derivedOperation(e.id, "project-revoked"), problemRevoked
				if tc.held {
					wantOp, wantProblem = e.cancelOp, ""
				}
				if rec.CancelOperation != wantOp || r.Problem != wantProblem {
					t.Fatalf("revoked %s: cancellation %s, problem %q", tc.state, e.opName(rec.CancelOperation), r.Problem)
				}
				assertOnlySealedUsageDeducted(t, *rec)
			})
		}
	}
}

// assertOnlySealedUsageDeducted checks that the execution's remaining
// allowance is its ceiling less the usage its visits sealed: nothing
// unsealed was refunded or deducted twice.
func assertOnlySealedUsageDeducted(t *testing.T, rec session.Record) {
	t.Helper()
	sealed := int64(0)
	for _, v := range rec.Visits {
		if v.Receipt == nil {
			t.Fatalf("visit %s has no sealed receipt", v.Launch.AllocationID)
		}
		sealed += v.Receipt.Usage[fixtureModel].Units
	}
	if len(rec.Visits) == 0 || sealed == 0 {
		t.Fatalf("no sealed usage: %d visits", len(rec.Visits))
	}
	ceiling, remaining := rec.Ceiling[fixtureModel].Units.Value, rec.Remaining[fixtureModel].Units.Value
	if remaining != ceiling-sealed {
		t.Fatalf("remaining %d units, want %d less %d sealed", remaining, ceiling, sealed)
	}
}

// The driver takes no task action for an interactive session, through real
// runs from every state it can be in, revoked or not, before or after its
// deadline: it never asks for a task's completion, never stops its writer
// gracefully (only its owner does) and never writes a task's tail.
func TestDriverTakesNoTaskActionForAnInteractiveSession(t *testing.T) {
	t.Parallel()
	for _, state := range journalStates() {
		for _, clock := range driverClocks {
			for _, revoked := range []bool{false, true} {
				if state.taskOnly || state.needDeadline && clock == clockNone {
					continue
				}
				t.Run(fmt.Sprintf("%s %s revoked=%t", state.name, clock, revoked), func(t *testing.T) {
					t.Parallel()
					e := newDriverEnv(t, true, clock)
					state.build(t, e)
					e.offset.Store(0)
					_ = e.p.counts()
					if revoked {
						e.revoke()
					}
					driveRounds(t, e)
					r, err := e.o.read(e.id)
					if err != nil {
						t.Fatal(err)
					}
					if r.Finished || r.Result != "" {
						t.Fatal("the driver wrote an interactive session's tail")
					}
					if calls := e.p.counts(); strings.Contains(calls, "stop=") {
						t.Fatalf("the driver stopped an interactive session's writer: %s", calls)
					}
					rec, err := e.o.execution(r)
					if err != nil {
						t.Fatal(err)
					}
					completion := derivedOperation(e.id, actionTaskComplete)
					if rec != nil && slices.ContainsFunc(rec.Operations, func(op session.Operation) bool { return op.ID == completion }) {
						t.Fatal("the driver asked for an interactive session's completion")
					}
				})
			}
		}
	}
}

// The completion wait clears a problem recorded earlier: while the task's
// turn runs, nothing is wrong with it. A wait that reports the turn pending
// does not pass through after, which clears a problem only on success, so
// the wait clears it itself.
func TestCompletionWaitClearsAnEarlierProblem(t *testing.T) {
	t.Parallel()
	e := newDriverEnv(t, false, clockNone)
	namedJournalState(t, "active").build(t, e)
	r, err := e.o.read(e.id)
	if err != nil {
		t.Fatal(err)
	}
	r.Problem = "an earlier failure"
	if err = e.o.save(r); err != nil {
		t.Fatal(err)
	}
	e.fixture(func(p *fixturePlacement) { p.turnPending = 1 })
	if n := e.o.step(t.Context(), e.id); n != nextAgain {
		t.Fatalf("completion wait: %s, want again", nextNames[n])
	}
	if r, err = e.o.read(e.id); err != nil || r.Problem != "" {
		t.Fatalf("problem after the completion wait: %q %v", r.Problem, err)
	}
}
