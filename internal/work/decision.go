package work

import (
	"fmt"
	"slices"
	"time"

	"github.com/andrewmccall/sproozi/internal/deadline"
	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session"
)

// action is what the driver does next for a session (ADR 0057, section 2).
type action string

const (
	// actNothing leaves the session: it is settled, or its owner or
	// "recover" acts next.
	actNothing action = "nothing"
	// actStart captures the input and gives the first execution to the
	// coordinator.
	actStart action = "start"
	// actRecoverStart prepares and activates the first visit. A failure
	// waits for "recover".
	actRecoverStart action = "recover-start"
	// actComplete is a task's completion wait: the stop the supervisor
	// carries out once the task's own turn completed, repeated until then.
	actComplete action = "complete"
	// actRecover retries an unsettled stop or cancellation, with the
	// backoff.
	actRecover action = "recover"
	// actFinish writes the task's tail.
	actFinish action = "finish"
	// actEndExpired cancels a session whose deadline passed before it
	// started, recording that problem.
	actEndExpired action = "end-expired"
	// actEndRevoked cancels a session whose project no longer admits this
	// installation, recording that problem.
	actEndRevoked action = "end-revoked"
)

// phaseNotReceived is the driver's phase for an execution the coordinator
// has not received yet.
const phaseNotReceived session.Phase = ""

// situation is what the driver decides from: the coordinator's phase of the
// current execution and the requests pending on it.
type situation struct {
	phase       session.Phase
	interactive bool
	// cancel: the coordinator holds a cancellation of the execution.
	cancel bool
	// revoked: the session's project no longer admits this installation.
	revoked bool
	// lateStart: the deadline passed before the first visit started. Before
	// the coordinator receives the execution, and while it starts, the
	// deadline has passed; once idle, the deadline ended its first visit
	// before it started (startEnded). Once a visit started, it is false.
	lateStart bool
}

// situationOf is the driver's situation for record r, whose current
// execution the coordinator holds as rec, nil when it has not received it.
func (o *Owner) situationOf(r record, rec *session.Record, now time.Time) situation {
	s := situation{interactive: r.Intent.Mode == ModeInteractive,
		revoked: !project.Member(o.config.Projects, r.Association.Project, o.config.Principal)}
	switch {
	case rec == nil:
		s.phase, s.lateStart = phaseNotReceived, deadline.At(r.First.Deadline).Passed(now)
	case rec.Phase.Starting():
		s.lateStart = deadline.At(rec.Deadline).Passed(now)
	case rec.Phase == session.PhaseIdle:
		s.lateStart = startEnded(*rec)
	}
	if rec != nil {
		s.phase, s.cancel = rec.Phase, rec.CancelOperation != ""
	}
	return s
}

// condition is a decision row's requirement on one fact: either value, true
// or false.
type condition int

const (
	either condition = iota
	isTrue
	isFalse
)

func (c condition) holds(v bool) bool { return c == either || (c == isTrue) == v }

// rowMode is a decision row's requirement on the session's mode.
type rowMode int

const (
	eitherMode rowMode = iota
	taskOnly
	interactiveOnly
)

func (m rowMode) holds(interactive bool) bool {
	return m == eitherMode || (m == interactiveOnly) == interactive
}

// decisionRow maps every situation it matches to one action.
type decisionRow struct {
	phases                     []session.Phase
	mode                       rowMode
	cancel, revoked, lateStart condition
	action                     action
}

func (row decisionRow) matches(s situation) bool {
	return slices.Contains(row.phases, s.phase) && row.mode.holds(s.interactive) &&
		row.cancel.holds(s.cancel) && row.revoked.holds(s.revoked) && row.lateStart.holds(s.lateStart)
}

// Phase groups the decision table names.
var (
	phasesStarting  = []session.Phase{session.PhasePreparing, session.PhaseActivating}
	phasesUnsettled = []session.Phase{session.PhaseStopping, session.PhaseUnknownStop, session.PhaseUnknownUsage,
		session.PhaseCancelling}
	phasesRunning = append(slices.Clone(phasesStarting), session.PhaseActive)
	// phasesCancelHeld is every phase but cancelled in which the
	// coordinator can hold a cancellation: carrying it out ends there, or
	// leaves the stop or the usage unknown.
	phasesCancelHeld = []session.Phase{session.PhaseUnknownStop, session.PhaseUnknownUsage, session.PhaseCancelling}
)

// decisions is the driver's whole decision: every situation that can occur
// matches exactly one row, so the order of the rows means nothing. A
// situation that cannot occur, such as a cancellation held in a phase the
// coordinator never leaves a cancellation in, matches at most one row.
//
// The driver only starts an interactive session: its owner stops and
// resumes it, and only "recover" retries its unsettled stop or cancellation
// (ADR 0057, section 8). Once its project is revoked its owner can take no
// action on it, "recover" included, so the driver settles it as it settles
// a task's (section 2). A task is driven to its tail. The project and the
// deadline end a session only before it settles: a revoked project ends any
// session the coordinator may still run that no cancellation already ends,
// and the deadline ends a task, or any session not yet received, only
// before its first visit started (ADR 0060).
var decisions = []decisionRow{
	{phases: []session.Phase{session.PhaseCancelled}, action: actNothing},
	{phases: []session.Phase{session.PhaseIdle}, mode: interactiveOnly, action: actNothing},
	{phases: []session.Phase{session.PhaseIdle}, mode: taskOnly, lateStart: isTrue, action: actEndExpired},
	{phases: []session.Phase{session.PhaseIdle}, mode: taskOnly, lateStart: isFalse, action: actFinish},

	{phases: phasesUnsettled, mode: interactiveOnly, revoked: isFalse, action: actNothing},
	{phases: phasesCancelHeld, mode: taskOnly, cancel: isTrue, action: actRecover},
	{phases: phasesCancelHeld, mode: interactiveOnly, cancel: isTrue, revoked: isTrue, action: actRecover},

	{phases: []session.Phase{phaseNotReceived}, revoked: isTrue, action: actEndRevoked},
	{phases: phasesRunning, cancel: isFalse, revoked: isTrue, action: actEndRevoked},
	{phases: phasesUnsettled, cancel: isFalse, revoked: isTrue, action: actEndRevoked},

	{phases: []session.Phase{phaseNotReceived}, revoked: isFalse, lateStart: isTrue, action: actEndExpired},
	{phases: []session.Phase{phaseNotReceived}, revoked: isFalse, lateStart: isFalse, action: actStart},
	{phases: phasesStarting, mode: taskOnly, cancel: isFalse, revoked: isFalse, lateStart: isTrue, action: actEndExpired},
	{phases: phasesStarting, mode: taskOnly, cancel: isFalse, revoked: isFalse, lateStart: isFalse, action: actRecoverStart},
	{phases: phasesStarting, mode: interactiveOnly, cancel: isFalse, revoked: isFalse, action: actRecoverStart},
	{phases: []session.Phase{session.PhaseActive}, mode: interactiveOnly, cancel: isFalse, revoked: isFalse, action: actNothing},
	{phases: []session.Phase{session.PhaseActive, session.PhaseStopping}, mode: taskOnly, cancel: isFalse, revoked: isFalse,
		action: actComplete},
	{phases: phasesCancelHeld, mode: taskOnly, cancel: isFalse, revoked: isFalse, action: actRecover},
}

// decide is the one action decisions maps s to. A situation that matches no
// row, or more than one, is a programming error: decide refuses it, and the
// driver records a problem and retries after the backoff.
func decide(s situation) (action, error) {
	var found []action
	for _, row := range decisions {
		if row.matches(s) {
			found = append(found, row.action)
		}
	}
	if len(found) != 1 {
		return actNothing, fmt.Errorf("work: %d driver decisions for %+v", len(found), s)
	}
	return found[0], nil
}
