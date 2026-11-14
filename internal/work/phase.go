package work

import (
	"slices"

	"github.com/andrewmccall/sproozi/internal/session"
)

// Phase is a session's public phase, reported in View. It is derived, never
// stored: the coordinator's phase of the current execution, combined with
// the catalogue's task and transfer tails. The words are the coordinator's
// (session.Phase), so a row reads the same whichever owner lists it, plus
// retained, failed, exporting and pulled for a stopped session.
type Phase string

const (
	phasePreparing    = Phase(session.PhasePreparing)
	phaseActivating   = Phase(session.PhaseActivating)
	phaseActive       = Phase(session.PhaseActive)
	phaseStopping     = Phase(session.PhaseStopping)
	phaseUnknownStop  = Phase(session.PhaseUnknownStop)
	phaseUnknownUsage = Phase(session.PhaseUnknownUsage)
	phaseCancelling   = Phase(session.PhaseCancelling)
	phaseCancelled    = Phase(session.PhaseCancelled)
	// PhaseRetained is a stopped session whose writer stop, sealed usage,
	// checkpoint and, for a task, bounded answer are kept.
	PhaseRetained Phase = "retained"
	// PhaseFailed is a stopped session whose harness exited with a failure,
	// or that stopped with no conversation to keep.
	PhaseFailed Phase = "failed"
	// PhaseExporting is a retained session a machine has begun to pull.
	PhaseExporting Phase = "exporting"
	// PhasePulled means a machine pulled the stopped session; this owner
	// keeps only its read-only record.
	PhasePulled Phase = "pulled"

	phaseRetained = PhaseRetained
)

// Phases is every public phase of remote work.
var Phases = []Phase{phasePreparing, phaseActivating, phaseActive, phaseStopping, phaseUnknownStop,
	phaseUnknownUsage, phaseCancelling, phaseCancelled, PhaseRetained, PhaseFailed, PhaseExporting, PhasePulled}

// Settled reports a phase in which nothing runs and nothing is left for the
// executor to finish.
func (p Phase) Settled() bool {
	return slices.Contains([]Phase{phaseCancelled, PhaseRetained, PhaseFailed, PhaseExporting, PhasePulled}, p)
}

// stopped reports a phase whose writer stopped with its usage sealed and its
// checkpoint kept, so it can seed fresh work or transfer to a home.
func (p Phase) stopped() bool {
	return p == PhaseRetained || p == PhaseExporting || p == PhasePulled
}

// derivePhase is the public phase of a session from its record and the
// coordinator's record of its current execution, nil when the coordinator
// has not received it yet:
//
//   - the transfer tail wins: pulled, then exporting;
//   - before the coordinator holds the execution, preparing, or cancelled
//     when the catalogue cancelled it there;
//   - idle is retained once the task's tail is written, failed when the
//     harness exited with a failure or nothing was kept, and stopping until
//     the tail is written. An interactive session has no tail: idle is
//     failed when its harness exited with a failure, otherwise retained,
//     even with no conversation, since resume restarts from its workspace;
//   - every other coordinator phase is reported as it is.
func derivePhase(r record, rec *session.Record) Phase {
	switch {
	case r.Pulled:
		return PhasePulled
	case r.Transfer != "":
		return PhaseExporting
	case rec == nil && r.CancelOperation != "":
		return phaseCancelled
	case rec == nil:
		return phasePreparing
	case rec.Phase != session.PhaseIdle:
		return Phase(rec.Phase)
	case r.Intent.Mode == ModeInteractive && lastExit(*rec).Failed():
		return PhaseFailed
	case r.Intent.Mode == ModeInteractive:
		return PhaseRetained
	case !r.Finished:
		return phaseStopping
	case rec.Checkpoint.Path == "" || lastExit(*rec).Failed():
		return PhaseFailed
	}
	return PhaseRetained
}
