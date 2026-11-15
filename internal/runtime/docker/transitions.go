package docker

import (
	"fmt"
	"slices"

	"github.com/andrewmccall/sproozi/internal/session"
)

// dockerPhase is where a Docker placement record is in its lifecycle. The
// record declares its phases, the operations each accepts and the flags each
// may carry (ADR 0058):
//
//	(none) --Prepare--> creating --configured--> prepared
//	prepared|activating|active --Activate--> activating --native active--> active
//	activating --Inspect--> active (an interrupted activation is completed)
//	prepared|activating|active|stopping --Stop--> stopping --sealed--> stopped
//	prepared|activating|active|stopping|aborting --Abort--> aborting --fenced--> aborted
//	creating --AbortPreparation--> preparation-aborting
//
// Stop of a stopped record, and Abort of a stopped or aborted one, replay the
// retained result. Removing and Removed are an ordered release sub-machine on
// top of stopped, aborted and preparation-aborting.
type dockerPhase string

const (
	phaseCreating            dockerPhase = "creating"
	phasePrepared            dockerPhase = "prepared"
	phaseActivating          dockerPhase = "activating"
	phaseActive              dockerPhase = "active"
	phaseStopping            dockerPhase = "stopping"
	phaseStopped             dockerPhase = "stopped"
	phaseAborting            dockerPhase = "aborting"
	phaseAborted             dockerPhase = "aborted"
	phasePreparationAborting dockerPhase = "preparation-aborting"
)

var dockerPhases = []dockerPhase{phaseCreating, phasePrepared, phaseActivating, phaseActive, phaseStopping,
	phaseStopped, phaseAborting, phaseAborted, phasePreparationAborting}

// placementOp names an operation checked against placementTransitions.
type placementOp string

const (
	opPrepare           placementOp = "Prepare"
	opActivate          placementOp = "Activate"
	opRecoverActivation placementOp = "Inspect activation"
	opStop              placementOp = "Stop"
	opAbort             placementOp = "Abort"
	opAbortPreparation  placementOp = "AbortPreparation"
	opRelease           placementOp = "release"
)

// placementTransitions lists the phases each operation accepts. Every
// operation checks it under the visit's operation lock before changing
// anything; a phase missing here is an explicit refusal.
var placementTransitions = map[placementOp][]dockerPhase{
	opPrepare:           {phaseCreating, phasePrepared},
	opActivate:          {phasePrepared, phaseActivating, phaseActive},
	opRecoverActivation: {phaseActivating},
	opStop:              {phasePrepared, phaseActivating, phaseActive, phaseStopping, phaseStopped},
	opAbort:             {phasePrepared, phaseActivating, phaseActive, phaseStopping, phaseAborting, phaseStopped, phaseAborted},
	// AbortPreparation accepts every phase: it moves a creating record to
	// preparation-aborting and hands a prepared or later one to Abort.
	opAbortPreparation: dockerPhases,
	opRelease:          {phaseStopped, phaseAborted, phasePreparationAborting},
}

// permit refuses op from a phase its table entry leaves out.
func (r placementRecord) permit(op placementOp) error {
	if !slices.Contains(placementTransitions[op], r.Phase) {
		return fmt.Errorf("docker placement: %s refused from phase %s", op, r.Phase)
	}
	return nil
}

// Stopping has three sub-states that decide what the stop retains: a visit
// whose harness ended its first visit with no conversation (turnless), one
// whose harness exited before the stop (exited), and one still to capture.

// writerKnown reports whether the stop knows which native thread the visit
// wrote, or that it wrote none: a turnless visit has no thread, and an exited
// one keeps the thread it reported, if any.
func (r placementRecord) writerKnown() bool {
	return validUUID(r.thread()) || r.Turnless || r.Exited != nil
}

// needsCapture reports whether the stop must still capture a checkpoint. A
// turnless visit keeps the workspace it started from. An exited harness keeps
// a checkpoint only when its state proves completion and it reported a
// thread; otherwise the session keeps its previous checkpoint.
func (r placementRecord) needsCapture(provesCompletion bool) bool {
	exitedWithout := r.Exited != nil && (!provesCompletion || r.thread() == "")
	return r.CheckpointDigest == "" && !r.Turnless && !exitedWithout
}

// stoppingOrLater are the phases in which a stop or abort has begun.
var stoppingOrLater = []dockerPhase{phaseStopping, phaseStopped, phaseAborting, phaseAborted}

// validateRecord checks the phase and the flags that each phase may carry.
// load and save both call it, so a record that contradicts itself fails
// closed instead of driving a transition.
func validateRecord(r placementRecord) error {
	if !slices.Contains(dockerPhases, r.Phase) {
		return invalidRecord(r, fmt.Sprintf("unknown phase %q", r.Phase))
	}
	for _, check := range []func(placementRecord) string{checkResources, checkStop, checkNative, checkReceipt, checkRelease} {
		if reason := check(r); reason != "" {
			return invalidRecord(r, reason)
		}
	}
	return nil
}

func invalidRecord(r placementRecord, reason string) error {
	return fmt.Errorf("docker placement record for execution %s is invalid: %s", r.Execution, reason)
}

// checkResources requires every container and the volume once preparation
// completed; only creating and preparation-aborting records may lack some.
func checkResources(r placementRecord) string {
	if r.Phase == phaseCreating || r.Phase == phasePreparationAborting {
		return ""
	}
	if r.Agent == nil || r.Helper == nil || r.Anchor == nil || r.Volume == nil {
		return string(r.Phase) + " without every container and its volume"
	}
	return ""
}

// checkStop keeps the actual stop and stop state to the phases that stop.
func checkStop(r placementRecord) string {
	inStop := slices.Contains(stoppingOrLater, r.Phase)
	switch {
	case r.ActualStopped && !inStop && r.Phase != phasePreparationAborting:
		return string(r.Phase) + " marked stopped"
	case (r.Phase == phaseStopped || r.Phase == phaseAborted) && !r.ActualStopped:
		return string(r.Phase) + " without an actual stop"
	case r.Phase == phaseStopped && r.Receipt == nil:
		return "stopped without a usage receipt"
	case !inStop && (r.NativeQuiet || r.Turnless || r.Exited != nil || r.Thread != "" || r.Receipt != nil || r.CheckpointDigest != ""):
		return string(r.Phase) + " carries stop state"
	}
	return ""
}

// checkNative keeps a bound thread to first visits and a turnless stop to a
// quiet first visit that never had a thread.
func checkNative(r placementRecord) string {
	if r.Thread != "" && (r.Launch.ThreadID != "" || !validUUID(r.Thread)) {
		return "bound thread on a resumed visit or not a native UUID"
	}
	if r.Turnless && (!r.NativeQuiet || !r.fresh() || r.thread() != "" || r.Exited != nil) {
		return "turnless visit that is not a quiet first visit without a thread"
	}
	return ""
}

// checkReceipt requires a receipt to follow the actual stop and name this
// visit, and a checkpoint to follow the receipt.
func checkReceipt(r placementRecord) string {
	if r.Receipt != nil {
		if !r.ActualStopped {
			return "usage receipt before the actual stop"
		}
		inc, err := r.incarnation()
		if err != nil || r.Receipt.Incarnation != inc || !session.SameLaunch(r.Receipt.Launch, r.Launch) {
			return "usage receipt for another visit"
		}
	}
	if r.CheckpointDigest != "" && (r.Receipt == nil || r.Turnless || !objectID.MatchString(r.CheckpointDigest)) {
		return "checkpoint without a receipt, on a turnless visit or with a malformed digest"
	}
	return ""
}

// checkRelease keeps the release sub-machine ordered: Removing only once the
// visit is releasable, and Removed only after Removing.
func checkRelease(r placementRecord) string {
	if r.Removing && !r.releasable() {
		return "removing a visit that cannot be released"
	}
	if r.Removed && !r.Removing {
		return "removed without removing"
	}
	return ""
}
