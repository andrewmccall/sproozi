package work

import (
	"slices"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session"
)

// Each derived phase is the coordinator's phase of the current execution,
// with idle shown as retained or failed once the task's tail is written, and
// the transfer tail winning.
func TestDerivedPhaseFollowsTheCoordinator(t *testing.T) {
	kept := session.Checkpoint{Path: "checkpoints/kept.json"}
	failed := []session.Visit{{Exit: &session.Exit{Code: 1}}}
	for _, tc := range []struct {
		name string
		r    record
		rec  *session.Record
		want Phase
	}{
		{"not received", record{}, nil, phasePreparing},
		{"cancelled before it started", record{CancelOperation: "op"}, nil, phaseCancelled},
		{"preparing", record{}, &session.Record{Phase: session.PhasePreparing}, phasePreparing},
		{"activating", record{}, &session.Record{Phase: session.PhaseActivating}, phaseActivating},
		{"active", record{}, &session.Record{Phase: session.PhaseActive}, phaseActive},
		{"stopping", record{}, &session.Record{Phase: session.PhaseStopping}, phaseStopping},
		{"unknown stop", record{}, &session.Record{Phase: session.PhaseUnknownStop}, phaseUnknownStop},
		{"unknown usage", record{}, &session.Record{Phase: session.PhaseUnknownUsage}, phaseUnknownUsage},
		{"cancelling", record{}, &session.Record{Phase: session.PhaseCancelling}, phaseCancelling},
		{"cancelled", record{}, &session.Record{Phase: session.PhaseCancelled}, phaseCancelled},
		{"idle before the tail", record{}, &session.Record{Phase: session.PhaseIdle, Checkpoint: kept}, phaseStopping},
		{"retained", record{Finished: true}, &session.Record{Phase: session.PhaseIdle, Checkpoint: kept}, PhaseRetained},
		{"failed exit", record{Finished: true}, &session.Record{Phase: session.PhaseIdle, Checkpoint: kept, Visits: failed}, PhaseFailed},
		{"nothing kept", record{Finished: true}, &session.Record{Phase: session.PhaseIdle}, PhaseFailed},
		{"exporting", record{Finished: true, Transfer: "op"}, &session.Record{Phase: session.PhaseIdle, Checkpoint: kept}, PhaseExporting},
		{"pulled", record{Finished: true, Transfer: "op", Pulled: true}, &session.Record{Phase: session.PhaseIdle, Checkpoint: kept}, PhasePulled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := derivePhase(tc.r, tc.rec)
			if got != tc.want || !slices.Contains(Phases, got) {
				t.Fatalf("phase %s, want %s", got, tc.want)
			}
			// Remote work's deadline applies in the same phases as the
			// coordinator's.
			if tc.rec != nil && tc.rec.Phase != session.PhaseIdle && session.Phase(got).Live() != tc.rec.Phase.Live() {
				t.Fatalf("%s live = %v, coordinator %s live = %v", got, session.Phase(got).Live(), tc.rec.Phase, tc.rec.Phase.Live())
			}
		})
	}
	for _, p := range Phases {
		if p.Settled() && session.Phase(p).Live() {
			t.Errorf("%s is settled, but its deadline still applies", p)
		}
	}
}
