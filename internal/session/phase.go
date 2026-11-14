package session

import (
	"errors"
	"fmt"
	"slices"

	"github.com/andrewmccall/sproozi/internal/deadline"
)

// Phase summarises an execution for callers and status output. It is the
// public phase vocabulary: home status, the CLI and ADR 0018's initialStage
// alias all report these words.
//
// The phase is derived from the record's structure (see describe) except for
// one fact the structure cannot hold: whether the last attempt to stop the
// writer failed to prove it stopped (PhaseUnknownStop). It is still stored, so
// journals and status keep it, and every load and save checks that the stored
// phase is the derived one. Only apply, following the transitions table,
// changes it.
type Phase string

const (
	// PhaseIdle has no writer and nothing in progress.
	PhaseIdle Phase = "idle"
	// PhasePreparing and PhaseActivating have a pending operation whose
	// destination is not yet active, and no other writer.
	PhasePreparing  Phase = "preparing"
	PhaseActivating Phase = "activating"
	// PhaseActive has one admitted writer and nothing in progress.
	PhaseActive Phase = "active"
	// PhaseStopping is a graceful stop of the active writer in progress.
	PhaseStopping Phase = "stopping"
	// PhaseUnknownStop is a stop or cancellation that could not prove the
	// writer stopped. The allocation stays encumbered.
	PhaseUnknownStop Phase = "unknown-stop"
	// PhaseUnknownUsage has a fenced writer whose usage is not sealed.
	PhaseUnknownUsage Phase = "unknown-usage"
	// PhaseCancelling has durable cancellation intent not yet carried out:
	// a writer may run, or a pending destination may have started one.
	PhaseCancelling Phase = "cancelling"
	// PhaseCancelled has carried out cancellation; nothing can start again.
	PhaseCancelled Phase = "cancelled"

	// phaseNone is the phase before Create retains a record.
	phaseNone Phase = ""
)

// phases is every phase a record can have.
var phases = []Phase{PhaseIdle, PhasePreparing, PhaseActivating, PhaseActive, PhaseStopping,
	PhaseUnknownStop, PhaseUnknownUsage, PhaseCancelling, PhaseCancelled}

func (p Phase) valid() bool {
	return slices.Contains(phases, p)
}

// Live reports an execution that is starting, running or stopping gracefully,
// so its deadline still applies. An unresolved stop and a cancellation are not
// live: recovery, not the deadline, decides what happens next.
func (p Phase) Live() bool { return p.Starting() || p == PhaseActive || p == PhaseStopping }

// Starting reports a first visit or a move whose destination is not yet active.
func (p Phase) Starting() bool { return p == PhasePreparing || p == PhaseActivating }

// Stopping reports a stop in progress or one that has not yet proved the
// writer stopped and its usage sealed. Repeating the stop continues it.
func (p Phase) Stopping() bool {
	return p == PhaseStopping || p == PhaseUnknownStop || p == PhaseUnknownUsage
}

// Unresolved reports an unproven stop or unfinished cancellation that only
// recovery (or a repeated stop) can finish; the allocation stays encumbered.
func (p Phase) Unresolved() bool {
	return p == PhaseUnknownStop || p == PhaseUnknownUsage || p == PhaseCancelling
}

// Settled reports an execution with no writer, nothing in progress and its
// usage sealed: idle, or cancelled.
func (p Phase) Settled() bool { return p == PhaseIdle || p == PhaseCancelled }

// event is a request to the coordinator or the outcome of a placement call.
type event string

const (
	// evCreate retains a new execution's first visit.
	evCreate event = "create"
	// evMove begins a move: stop the writer, or prepare when none runs.
	evMove event = "move"
	// evFinalize begins a stop that starts no destination.
	evFinalize event = "finalize"
	// evStopped: Stop fenced the writer and its sealed receipt was retained.
	evStopped event = "stopped"
	// evStopUnproven: Stop, Abort, AbortPreparation or Inspect did not prove
	// the writer stopped. A writer already proven stopped stays so.
	evStopUnproven event = "stop-unproven"
	// evUsageUnsealed: the writer is fenced but its usage is unsealed or its
	// receipt is not valid.
	evUsageUnsealed event = "usage-unsealed"
	// evPrepared: the destination reported a prepared incarnation.
	evPrepared event = "prepared"
	// evActivated: the destination's writer was admitted.
	evActivated event = "activated"
	// evFinalized: a finalization's stop committed; nothing starts.
	evFinalized event = "finalized"
	// evCancel retains cancellation intent.
	evCancel event = "cancel"
	// evCancelled: no writer can run and its usage is sealed.
	evCancelled event = "cancelled"
)

// rule is what an event may do.
type rule struct {
	// refusedAfterDeadline: the coordinator does not attempt the event once
	// the execution's deadline has passed, because it would start or extend
	// work. Stopping, accounting and cancelling are always allowed. An
	// execution without a deadline refuses nothing (ADR 0056).
	refusedAfterDeadline bool
	// to maps each phase the event may happen in to the phase it leads to.
	to map[Phase]Phase
}

// transitions declares every phase change the coordinator makes. Recovery
// uses the same events as the live operations it resumes.
var transitions = map[event]rule{
	evCreate:   {refusedAfterDeadline: true, to: map[Phase]Phase{phaseNone: PhasePreparing}},
	evMove:     {to: map[Phase]Phase{PhaseIdle: PhasePreparing, PhaseActive: PhaseStopping}},
	evFinalize: {to: map[Phase]Phase{PhaseIdle: PhaseIdle, PhaseActive: PhaseStopping}},
	evStopped: {to: map[Phase]Phase{
		PhaseStopping: PhasePreparing, PhaseUnknownStop: PhasePreparing, PhaseUnknownUsage: PhasePreparing}},
	evStopUnproven: {to: map[Phase]Phase{
		PhaseStopping: PhaseUnknownStop, PhaseUnknownStop: PhaseUnknownStop, PhaseCancelling: PhaseUnknownStop,
		PhaseUnknownUsage: PhaseUnknownUsage}},
	evUsageUnsealed: {to: map[Phase]Phase{
		PhaseStopping: PhaseUnknownUsage, PhaseUnknownStop: PhaseUnknownUsage, PhaseUnknownUsage: PhaseUnknownUsage,
		PhaseCancelling: PhaseUnknownUsage}},
	evPrepared:  {refusedAfterDeadline: true, to: map[Phase]Phase{PhasePreparing: PhaseActivating}},
	evActivated: {refusedAfterDeadline: true, to: map[Phase]Phase{PhaseActivating: PhaseActive}},
	evFinalized: {to: map[Phase]Phase{PhasePreparing: PhaseIdle}},
	evCancel: {to: map[Phase]Phase{
		PhaseIdle: PhaseCancelled, PhasePreparing: PhaseCancelling, PhaseActivating: PhaseCancelling,
		PhaseActive: PhaseCancelling, PhaseStopping: PhaseCancelling, PhaseUnknownStop: PhaseCancelling,
		PhaseUnknownUsage: PhaseUnknownUsage}},
	evCancelled: {to: map[Phase]Phase{
		PhaseCancelling: PhaseCancelled, PhaseUnknownStop: PhaseCancelled, PhaseUnknownUsage: PhaseCancelled}},
}

// admit checks, before the coordinator attempts ev's side effect, that ev may
// happen in r's phase and, for an event that starts work, that the
// execution's deadline has not passed.
func (c *Coordinator) admit(r Record, ev event) error {
	if _, ok := transitions[ev].to[r.Phase]; !ok {
		return refused(r.Phase, ev)
	}
	if transitions[ev].refusedAfterDeadline && deadline.At(r.Deadline).Passed(c.now()) {
		return errors.New("session: original deadline expired")
	}
	return nil
}

// apply records ev: it moves r to the phase the transitions table declares,
// or refuses an undeclared transition and leaves r unchanged. A refusal is a
// programming error; the caller must not save the record. save then checks
// that the phase describes the record's structure.
func apply(r *Record, ev event) error {
	to, ok := transitions[ev].to[r.Phase]
	if !ok {
		return refused(r.Phase, ev)
	}
	r.Phase = to
	return nil
}

func refused(from Phase, ev event) error {
	if from == phaseNone {
		from = "none"
	}
	return fmt.Errorf("session: transition refused: %s in phase %s", ev, from)
}

// commit applies ev and saves the record.
func (c *Coordinator) commit(r *Record, ev event) error {
	if err := apply(r, ev); err != nil {
		return err
	}
	return c.save(*r)
}

// pendingStage is the next step of the pending operation, or "" when none.
func pendingStage(r Record) Stage {
	if r.PendingOperation == "" {
		return ""
	}
	return r.Operations[len(r.Operations)-1].Stage
}

// describe is the one phase r's structure describes. stopUnproven is the fact
// the structure cannot hold: the last attempt to stop the writer did not prove
// it stopped. It returns "" for a structure no phase describes.
func describe(r Record, stopUnproven bool) Phase {
	pending := pendingStage(r)
	cancel := r.CancelOperation != ""
	writer := r.Active != nil
	fenced := writer && r.Active.ActualStopped
	running := writer && !fenced
	var p Phase
	switch {
	case fenced:
		// A writer proven stopped stays so; only its usage is unknown.
		if pending == stageStop || cancel {
			return PhaseUnknownUsage
		}
		return ""
	case cancel && !writer && pending == "":
		p = PhaseCancelled
	case cancel:
		p = PhaseCancelling
	case running && pending == stageStop:
		p = PhaseStopping
	case running && pending == "":
		p = PhaseActive
	case running:
	case pending == "":
		p = PhaseIdle
	case pending == stagePrepare:
		p = PhasePreparing
	case pending == stageActivate:
		p = PhaseActivating
	}
	if stopUnproven {
		if p == PhaseCancelling || p == PhaseStopping {
			return PhaseUnknownStop
		}
		return ""
	}
	return p
}

// validatePhase checks that the stored phase is the one the record's
// structure describes: whether a writer may run (Active), whether it is
// fenced (ActualStopped), which step is pending and whether cancellation was
// requested.
func validatePhase(r Record) error {
	if r.CancelOperation != "" && !validUUID(r.CancelOperation) {
		return errors.New("session: invalid cancellation intent")
	}
	if !r.Phase.valid() {
		return fmt.Errorf("session: invalid phase %q", r.Phase)
	}
	if describe(r, r.Phase == PhaseUnknownStop) != r.Phase {
		return fmt.Errorf("session: phase %q contradicts the retained execution", r.Phase)
	}
	return nil
}

// normalizeLegacyPhase reads a journal written while cancelling could
// describe any cancelled record: it becomes the phase its structure
// describes. Recovery carries out the cancellation from the structure alone,
// so the old word lost nothing the new one needs.
func normalizeLegacyPhase(r *Record) {
	if r.Phase == PhaseCancelling && r.CancelOperation != "" {
		if derived := describe(*r, false); derived != "" {
			r.Phase = derived
		}
	}
}
