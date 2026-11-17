package work

import (
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/google/uuid"
)

// driverPhases is every phase the driver decides from: not received, then
// each phase of the execution journal. A phase added to the journal is a
// situation no row decides, so the table test fails until a row does.
var driverPhases = append([]session.Phase{phaseNotReceived}, session.Phases()...)

var driverActions = []action{actNothing, actStart, actRecoverStart, actComplete, actRecover, actFinish,
	actEndExpired, actEndRevoked}

// allSituations is every combination of phase, mode and pending request.
func allSituations() []situation {
	bools := []bool{false, true}
	all := make([]situation, 0, len(driverPhases)*16)
	for _, phase := range driverPhases {
		for _, interactive := range bools {
			for _, cancel := range bools {
				for _, revoked := range bools {
					for _, late := range bools {
						all = append(all, situation{phase: phase, interactive: interactive, cancel: cancel,
							revoked: revoked, lateStart: late})
					}
				}
			}
		}
	}
	return all
}

// Every situation that can occur matches exactly one row, so no decision
// depends on the order of the rows or falls through to a default; one that
// cannot occur matches at most one. Every row decides some situation that
// can occur, and names a known phase and action.
func TestDecisionTableDecidesEverySituationOnce(t *testing.T) {
	used := make([]bool, len(decisions))
	for _, s := range allSituations() {
		var rows []int
		for i, row := range decisions {
			if row.matches(s) {
				rows = append(rows, i)
				used[i] = used[i] || possible(s)
			}
		}
		if possible(s) && len(rows) != 1 || len(rows) > 1 {
			t.Errorf("%+v (possible %t) matches rows %v", s, possible(s), rows)
		}
		if _, err := decide(s); (err == nil) != (len(rows) == 1) {
			t.Errorf("decide(%+v): %v", s, err)
		}
	}
	for i, row := range decisions {
		if !used[i] {
			t.Errorf("row %d decides nothing that can occur", i)
		}
		if !slices.Contains(driverActions, row.action) {
			t.Errorf("row %d: unknown action %q", i, row.action)
		}
		for _, p := range row.phases {
			if !slices.Contains(driverPhases, p) {
				t.Errorf("row %d: unknown phase %q", i, p)
			}
		}
	}
	for _, a := range driverActions {
		if !slices.ContainsFunc(decisions, func(row decisionRow) bool { return row.action == a }) {
			t.Errorf("action %s is never taken", a)
		}
	}
}

// possible reports a situation the coordinator and situationOf can produce.
// A cancellation the coordinator holds is carried out at once, so the
// journal's structure (describe in internal/session/phase.go) holds one only
// in cancelling, cancelled, unknown-stop and unknown-usage, and always in
// cancelling and cancelled. situationOf sets lateStart only before the first
// visit started or once it ended idle.
func possible(s situation) bool {
	held := []session.Phase{session.PhaseCancelling, session.PhaseCancelled, session.PhaseUnknownStop,
		session.PhaseUnknownUsage}
	switch {
	case s.cancel && !slices.Contains(held, s.phase):
		return false
	case !s.cancel && (s.phase == session.PhaseCancelling || s.phase == session.PhaseCancelled):
		return false
	}
	return !s.lateStart || s.phase == phaseNotReceived || s.phase.Starting() || s.phase == session.PhaseIdle
}

// The rules the table encodes, for every situation that can occur. The
// driver only starts an interactive session (ADR 0057, section 8), unless
// its project is revoked: then it settles it as a task's (section 2), but
// never completes or finishes it. It does nothing only for a cancelled
// execution, or an interactive session whose owner or "recover" acts next;
// every other phase has an action that makes progress. A cancellation the
// coordinator holds is carried out, never replaced by the project's, and
// for a task the project does not change what happens to it. The deadline
// matters only before the first visit started (ADR 0060).
func TestDecisionTableRules(t *testing.T) {
	for _, s := range allSituations() {
		if !possible(s) {
			continue
		}
		act, err := decide(s)
		if err != nil {
			t.Fatal(err)
		}
		checkProgressRules(t, s, act)
		checkRequestRules(t, s, act)
	}
}

// checkProgressRules checks who acts on s: the driver for a task, its owner
// for an interactive session whose project admits this installation.
func checkProgressRules(t *testing.T, s situation, act action) {
	t.Helper()
	ownerActs := []action{actNothing, actStart, actRecoverStart, actEndExpired, actEndRevoked}
	if s.interactive && !s.revoked && !slices.Contains(ownerActs, act) ||
		s.interactive && (act == actComplete || act == actFinish) {
		t.Errorf("%+v: the driver takes %s for an interactive session", s, act)
	}
	if act == actNothing && s.phase != session.PhaseCancelled && !s.interactive {
		t.Errorf("%+v: a task makes no progress", s)
	}
	if s.phase == session.PhaseCancelled && act != actNothing {
		t.Errorf("%+v: a cancelled execution is driven: %s", s, act)
	}
	// carryOut reads the coordinator's record for every other action.
	if s.phase == phaseNotReceived && !slices.Contains([]action{actStart, actEndExpired, actEndRevoked}, act) {
		t.Errorf("%+v: %s needs an execution the coordinator has not received", s, act)
	}
}

// checkRequestRules checks how a held cancellation, a revoked project and a
// late start decide s.
func checkRequestRules(t *testing.T, s situation, act action) {
	t.Helper()
	if s.cancel && (act == actEndRevoked || act == actEndExpired) {
		t.Errorf("%+v: a held cancellation is replaced: %s", s, act)
	}
	if s.cancel && !s.interactive {
		other := s
		other.revoked = !s.revoked
		if again, _ := decide(other); again != act {
			t.Errorf("%+v: the project changes a task's held cancellation: %s, %s", s, act, again)
		}
	}
	if s.phase != phaseNotReceived && !s.phase.Starting() && s.phase != session.PhaseIdle {
		other := s
		other.lateStart = !s.lateStart
		if again, _ := decide(other); again != act {
			t.Errorf("%+v: the deadline decides after the start: %s, %s", s, act, again)
		}
	}
	if act == actEndExpired && !s.lateStart || act == actEndRevoked && !s.revoked {
		t.Errorf("%+v: ends without its reason: %s", s, act)
	}
}

// settle takes the driver's steps, and "recover" for an interactive
// session, and for the stop path the owner's stop of a running interactive
// session, until the session settles. It reports the settled phase, or the
// phase it was left in.
func settle(t *testing.T, e *driverEnv, interactive, stop bool) Phase {
	t.Helper()
	for range 8 {
		v, err := e.o.Info(t.Context(), e.id)
		if err != nil {
			t.Fatal(err)
		}
		if v.Phase.Settled() {
			return v.Phase
		}
		if interactive && stop && (v.Phase == phaseActive || v.Phase == phaseStopping) {
			_, _ = e.o.Stop(t.Context(), e.id, uuid.NewString())
		}
		for range driverStepLimit {
			if e.o.step(t.Context(), e.id) != nextAgain {
				break
			}
		}
		if interactive {
			_, _ = e.o.Recover(t.Context(), e.id)
		}
	}
	v, err := e.o.Info(t.Context(), e.id)
	if err != nil {
		t.Fatal(err)
	}
	return v.Phase
}

// From every phase the coordinator can hold, a session with no deadline
// (ADR 0056) and one whose deadline has passed can each still be stopped and
// cancelled: driven, recovered and, for an interactive session, stopped by
// its owner, it settles. Stopping ends in a cancellation only for a session
// the coordinator already held a cancellation of, or whose deadline passed
// before its first visit started. Cancelling ends cancelled, or is refused
// for a session that already stopped and keeps its result. Every phase of
// the execution journal is reached.
func TestEveryDriverPhaseCanStillBeStoppedAndCancelled(t *testing.T) {
	var mu sync.Mutex
	reached := map[session.Phase]bool{}
	t.Run("cases", func(t *testing.T) {
		for _, state := range journalStates() {
			for _, interactive := range []bool{false, true} {
				for _, clock := range []driverClock{clockLate, clockNone} {
					for _, stop := range []bool{true, false} {
						c := driverCase{state: state, interactive: interactive, clock: clock}
						if state.taskOnly && interactive || state.needDeadline && clock == clockNone {
							continue
						}
						t.Run(fmt.Sprintf("%s stop=%t", c, stop), func(t *testing.T) {
							t.Parallel()
							e := newDriverEnv(t, interactive, clock)
							state.build(t, e)
							e.offset.Store(0)
							r, err := e.o.read(e.id)
							if err != nil {
								t.Fatal(err)
							}
							rec, err := e.o.execution(r)
							if err != nil {
								t.Fatal(err)
							}
							s := e.o.situationOf(r, rec)
							mu.Lock()
							reached[s.phase] = true
							mu.Unlock()
							before, err := e.o.Info(t.Context(), e.id)
							if err != nil {
								t.Fatal(err)
							}
							var cancelErr error
							if !stop {
								_, cancelErr = e.o.Cancel(t.Context(), e.id, uuid.NewString())
							}
							got := settle(t, e, interactive, stop)
							switch {
							case !got.Settled():
								t.Fatalf("from %s the session is left %s", before.Phase, got)
							case stop && got == phaseCancelled && !s.cancel && !s.lateStart && s.phase != session.PhaseCancelled:
								t.Fatalf("stopping from %s cancelled it", before.Phase)
							case !stop && got != phaseCancelled && (code(cancelErr) != profile.CodeConflict || !got.stopped() && got != PhaseFailed):
								t.Fatalf("cancelling from %s: %s, %v", before.Phase, got, cancelErr)
							}
						})
					}
				}
			}
		}
	})
	for _, p := range driverPhases {
		if !reached[p] {
			t.Errorf("phase %q is never reached", p)
		}
	}
}

// A situation no row decides is not left silently: the driver records a
// problem the owner sees and retries after the backoff, and changes
// nothing else. It runs alone, since it replaces the package's table.
func TestUndecidedSituationIsReportedAndRetried(t *testing.T) {
	e := newDriverEnv(t, false, clockNone)
	e.activate(t)
	table := decisions
	decisions = nil
	t.Cleanup(func() { decisions = table })
	if n := e.o.step(t.Context(), e.id); n != nextWait {
		t.Fatalf("undecided step: %s, want wait", nextNames[n])
	}
	r, err := e.o.read(e.id)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := e.o.execution(r)
	if err != nil {
		t.Fatal(err)
	}
	if r.Problem != problemUndecided || rec.Phase != session.PhaseActive || r.Finished {
		t.Fatalf("undecided session: problem %q, phase %s, finished %t", r.Problem, rec.Phase, r.Finished)
	}
	if calls := e.p.counts(); calls != "calls=prepare=1,activate=1,inspect=2" {
		t.Fatalf("undecided step called the placement: %s", calls)
	}
}
