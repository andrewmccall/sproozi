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
	interactiveRecord := record{Intent: Submit{Mode: ModeInteractive}}
	// pending is an execution whose stop, finalizing op, is in phase.
	const execution = "6f1d3c55-5c1b-4f55-9d0e-0d4b7b8f2a10"
	pending := func(phase session.Phase, op string) *session.Record {
		return &session.Record{ID: execution, Phase: phase, PendingOperation: op,
			Operations: []session.Operation{{ID: op, Finalize: true}}}
	}
	completion := derivedOperation(execution, actionTaskComplete)
	callerStop := "0b8e1f9c-58a4-4c3e-a3b6-2a3c9d1e7f42"
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
		// An interactive session has no task tail.
		{"interactive retained", interactiveRecord, &session.Record{Phase: session.PhaseIdle, Checkpoint: kept}, PhaseRetained},
		{"interactive turnless", interactiveRecord, &session.Record{Phase: session.PhaseIdle}, PhaseRetained},
		{"interactive failed exit", interactiveRecord, &session.Record{Phase: session.PhaseIdle, Checkpoint: kept, Visits: failed}, PhaseFailed},
		{"interactive active", interactiveRecord, &session.Record{Phase: session.PhaseActive}, phaseActive},
		// A task's own completion wait is not a stop: it runs until its
		// turn completes, then stops.
		{"task waiting for its turn", record{}, pending(session.PhaseStopping, completion), phaseActive},
		{"task stopped after its turn", record{}, pending(session.PhasePreparing, completion), phaseStopping},
		{"task completion unproven", record{}, pending(session.PhaseUnknownStop, completion), phaseUnknownStop},
		{"task completion unsealed", record{}, pending(session.PhaseUnknownUsage, completion), phaseUnknownUsage},
		// A stop a caller asked for is reported as one.
		{"interactive stop by its owner", interactiveRecord, pending(session.PhaseStopping, callerStop), phaseStopping},
		{"interactive completion name", interactiveRecord, pending(session.PhaseStopping, completion), phaseStopping},
		{"task stop by another operation", record{}, pending(session.PhaseStopping, callerStop), phaseStopping},
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
