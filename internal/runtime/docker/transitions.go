package docker

import (
	"fmt"
	"slices"
	"time"

	"github.com/andrewmccall/sproozi/internal/deadline"
	"github.com/andrewmccall/sproozi/internal/session"
)

// dockerPhase is where a Docker placement record is in its lifecycle. The
// record declares its phases, the events that move it (placementTransitions)
// and the flags each phase may carry (ADR 0058).
//
// An operation on a record that already holds its outcome replays it without
// an event: Prepare of a prepared record, Activate of an active one, Stop of a
// stopped one and Abort of a stopped or aborted one. Removing and Removed are
// an ordered release sub-machine on top of stopped, aborted and
// preparation-aborting.
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

	// phaseNone is the phase before Prepare retains a record.
	phaseNone dockerPhase = ""
)

var dockerPhases = []dockerPhase{phaseCreating, phasePrepared, phaseActivating, phaseActive, phaseStopping,
	phaseStopped, phaseAborting, phaseAborted, phasePreparationAborting}

// placementEvent is a request to the placement or the outcome of its work.
type placementEvent string

const (
	// evCreate retains a new visit's record.
	evCreate placementEvent = "create"
	// evPrepared: the containers exist and the native supervisor is configured.
	evPrepared placementEvent = "prepared"
	// evActivate begins, or after an interruption resumes, activation.
	evActivate placementEvent = "activate"
	// evActivated: the grant and the native gate are active.
	evActivated placementEvent = "activated"
	// evAttach connects a terminal to the active visit; the phase stays.
	evAttach placementEvent = "attach"
	// evStop begins, or resumes, a graceful stop.
	evStop placementEvent = "stop"
	// evStopped: the writer is fenced, its usage sealed and its checkpoint kept.
	evStopped placementEvent = "stopped"
	// evAbort begins, or resumes, a cancellation of a prepared or later visit.
	evAbort placementEvent = "abort"
	// evAborted: the writer is fenced; its usage is sealed when it could be.
	evAborted placementEvent = "aborted"
	// evAbortPreparation cancels, or resumes cancelling, a preparation that
	// never completed.
	evAbortPreparation placementEvent = "abort-preparation"
)

// placementRule is what an event may do.
type placementRule struct {
	// refusedAfterDeadline: the event starts or extends work, so it is not
	// attempted once the launch's deadline has passed. A launch without a
	// deadline refuses nothing (ADR 0056).
	refusedAfterDeadline bool
	// to maps each phase the event may happen in to the phase it leads to.
	to map[dockerPhase]dockerPhase
}

// placementTransitions declares every phase change the placement makes.
// Inspect resumes an interrupted activation with the same events as Activate.
var placementTransitions = map[placementEvent]placementRule{
	evCreate:    {refusedAfterDeadline: true, to: map[dockerPhase]dockerPhase{phaseNone: phaseCreating}},
	evPrepared:  {refusedAfterDeadline: true, to: map[dockerPhase]dockerPhase{phaseCreating: phasePrepared}},
	evActivate:  {refusedAfterDeadline: true, to: map[dockerPhase]dockerPhase{phasePrepared: phaseActivating, phaseActivating: phaseActivating}},
	evActivated: {to: map[dockerPhase]dockerPhase{phaseActivating: phaseActive}},
	evAttach:    {refusedAfterDeadline: true, to: map[dockerPhase]dockerPhase{phaseActive: phaseActive}},
	evStop: {to: map[dockerPhase]dockerPhase{
		phasePrepared: phaseStopping, phaseActivating: phaseStopping, phaseActive: phaseStopping, phaseStopping: phaseStopping}},
	evStopped: {to: map[dockerPhase]dockerPhase{phaseStopping: phaseStopped}},
	evAbort: {to: map[dockerPhase]dockerPhase{
		phasePrepared: phaseAborting, phaseActivating: phaseAborting, phaseActive: phaseAborting, phaseStopping: phaseAborting,
		phaseAborting: phaseAborting}},
	evAborted: {to: map[dockerPhase]dockerPhase{phaseAborting: phaseAborted}},
	evAbortPreparation: {to: map[dockerPhase]dockerPhase{
		phaseCreating: phasePreparationAborting, phasePreparationAborting: phasePreparationAborting}},
}

// admit checks, before the placement attempts ev's side effect, that ev may
// happen in r's phase and, for an event that starts work, that the launch's
// deadline has not passed at now.
func (r placementRecord) admit(ev placementEvent, now time.Time) error {
	rule := placementTransitions[ev]
	if _, ok := rule.to[r.Phase]; !ok {
		return refused(r.Phase, ev)
	}
	if rule.refusedAfterDeadline && deadline.At(r.Launch.Deadline).Passed(now) {
		return fmt.Errorf("docker placement: %s refused after the launch deadline", ev)
	}
	return nil
}

// apply records ev: it moves r to the phase placementTransitions declares, or
// refuses an undeclared transition and leaves r unchanged. It is the only
// place a placement record's phase changes; save then validates the record.
func (r *placementRecord) apply(ev placementEvent) error {
	to, ok := placementTransitions[ev].to[r.Phase]
	if !ok {
		return refused(r.Phase, ev)
	}
	r.Phase = to
	return nil
}

func refused(from dockerPhase, ev placementEvent) error {
	if from == phaseNone {
		return fmt.Errorf("docker placement: %s refused without a record", ev)
	}
	return fmt.Errorf("docker placement: %s refused from phase %s", ev, from)
}

// commit applies ev and saves the record.
func (p *Placement) commit(r *placementRecord, ev placementEvent) error {
	if err := r.apply(ev); err != nil {
		return err
	}
	return p.save(*r)
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
