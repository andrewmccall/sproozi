package registry

import (
	"fmt"
	"time"

	"github.com/andrewmccall/sproozi/internal/deadline"
)

// Phase is where a session registration is in its lifecycle. The registration
// declares its phases, the events that move it (registrationTransitions) and
// the bound run, Pod and receipt each phase may carry (validateStored), as
// ADR 0058 requires of a lifecycle record.
//
// An operation on a registration that already holds its outcome replays it
// without an event: Create of an existing registration with the same intent,
// BindRun of the bound run, BindPod of the bound Pod, Prepared of a Pod
// already restored into, Activate of an active registration, BindNative of
// the bound native UUID, Stop of a stopping or stopped one, Cancel of a
// cancelled or stopped one, and CompleteStop or CompleteCancel with the
// receipt already retained.
type Phase string

const (
	Creating  Phase = "creating"
	Bound     Phase = "bound"
	Prepared  Phase = "prepared"
	Active    Phase = "active"
	Stopping  Phase = "stopping"
	Stopped   Phase = "stopped"
	Cancelled Phase = "cancelled"

	// phaseNone is the phase before Create retains a registration.
	phaseNone Phase = ""
)

var phases = []Phase{Creating, Bound, Prepared, Active, Stopping, Stopped, Cancelled}

// registrationEvent is a request to the registry or a record of an outcome
// the remote executor observed.
type registrationEvent string

const (
	// evCreate retains a new registration for an intended run.
	evCreate registrationEvent = "create"
	// evBindRun records the exact run created for the registration.
	evBindRun registrationEvent = "bind-run"
	// evBindPod records the exact Pod observed for the bound run. A
	// preparation cancelled after its run was bound records a Pod that
	// appears afterwards, so the placement can fence it.
	evBindPod registrationEvent = "bind-pod"
	// evPrepared: the checkpoint or workspace was restored into that Pod.
	evPrepared registrationEvent = "prepared"
	// evActivate grants the visit the gateway's authority.
	evActivate registrationEvent = "activate"
	// evBindNative records the native UUID a first visit's harness started.
	evBindNative registrationEvent = "bind-native"
	// evStop begins a graceful stop.
	evStop registrationEvent = "stop"
	// evCompleteStop: the native container ended and its checkpoint and
	// finalized usage are retained.
	evCompleteStop registrationEvent = "complete-stop"
	// evCancel ends the registration without a graceful stop.
	evCancel registrationEvent = "cancel"
	// evCompleteCancel: the exact Pod was fenced and its usage sealed.
	evCompleteCancel registrationEvent = "complete-cancel"
)

// registrationRule is what an event may do.
type registrationRule struct {
	// refusedAfterDeadline: the event starts or extends work, so it is not
	// recorded once the registration's original deadline has passed. A
	// registration without a deadline refuses nothing (ADR 0056).
	refusedAfterDeadline bool
	// to maps each phase the event may happen in to the phase it leads to.
	to map[Phase]Phase
}

// registrationTransitions declares every phase change the registry makes.
// Only create, which starts an execution, and activate, which grants it the
// gateway's authority, start work. The others record an outcome the remote
// executor already observed (a run created, a Pod scheduled, a restore
// completed, a native UUID started), or stop, cancel or account for work, so
// the deadline never refuses them.
var registrationTransitions = map[registrationEvent]registrationRule{
	evCreate:       {refusedAfterDeadline: true, to: map[Phase]Phase{phaseNone: Creating}},
	evBindRun:      {to: map[Phase]Phase{Creating: Bound}},
	evBindPod:      {to: map[Phase]Phase{Bound: Bound, Cancelled: Cancelled}},
	evPrepared:     {to: map[Phase]Phase{Bound: Prepared}},
	evActivate:     {refusedAfterDeadline: true, to: map[Phase]Phase{Prepared: Active}},
	evBindNative:   {to: map[Phase]Phase{Active: Active}},
	evStop:         {to: map[Phase]Phase{Active: Stopping}},
	evCompleteStop: {to: map[Phase]Phase{Stopping: Stopped}},
	evCancel: {to: map[Phase]Phase{
		Creating: Cancelled, Bound: Cancelled, Prepared: Cancelled, Active: Cancelled, Stopping: Cancelled}},
	evCompleteCancel: {to: map[Phase]Phase{Cancelled: Cancelled}},
}

// admit checks that ev may be recorded on r at now: for an event that starts
// work, that the registration's deadline has not passed, and that ev may
// happen in r's phase. The deadline is checked first, so an expired
// activation reports ErrExpired from any phase, as it always has.
func (r stored) admit(ev registrationEvent, now time.Time) error {
	rule := registrationTransitions[ev]
	if rule.refusedAfterDeadline && deadline.At(r.Registration.OriginalDeadline).Passed(now) {
		return fmt.Errorf("%w: %s refused after the registration deadline", ErrExpired, ev)
	}
	if _, ok := rule.to[r.Phase]; !ok {
		return refused(r.Phase, ev)
	}
	return nil
}

// apply records ev: it moves r to the phase registrationTransitions declares,
// or refuses an undeclared transition and leaves r unchanged. It is the only
// place a registration's phase changes; encode then validates the record.
func (r *stored) apply(ev registrationEvent) error {
	to, ok := registrationTransitions[ev].to[r.Phase]
	if !ok {
		return refused(r.Phase, ev)
	}
	r.Phase = to
	return nil
}

func refused(from Phase, ev registrationEvent) error {
	if from == phaseNone {
		return fmt.Errorf("%w: %s refused without a registration", ErrConflict, ev)
	}
	return fmt.Errorf("%w: %s refused from phase %s", ErrConflict, ev, from)
}
