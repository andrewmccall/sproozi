package work

import (
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

// An interactive session whose stop or cancellation is unsettled when its
// project is revoked is settled by the driver, as a task's is (ADR 0057,
// section 2): its owner can no longer act on it, so "recover" cannot. A
// cancellation already held is carried out; otherwise the session is
// cancelled for the revoked project. Either way the writer is fenced and the
// allocation released, and only sealed usage is deducted.
func TestRevokedUnsettledInteractiveSessionIsSettled(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		state string
		held  bool
	}{
		{"stopping", false},
		{"unknown-stop", false},
		{"unknown-usage", false},
		{"cancelling", true},
		{"cancel-unknown-stop", true},
		{"cancel-unknown-usage", true},
	} {
		t.Run(tc.state, func(t *testing.T) {
			t.Parallel()
			e := newDriverEnv(t, true, clockNone)
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
