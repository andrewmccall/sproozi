package work

import (
	"fmt"
	"slices"

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
	// actEndRefused cancels a task whose placement refused its start for
	// good, keeping the problem that names why (ADR 0067).
	actEndRefused action = "end-refused"
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
	// refused: the placement refused the first visit's start for good
	// (ADR 0067). Only an idle execution whose first visit ended before it
	// started has it, never with lateStart: the coordinator ends a start
	// only for one or the other, and only for its deadline once that has
	// passed. Before the deadline, or with none, the placement refused it;
	// past it, the refusal problem the driver recorded says which.
	refused bool
}

// situationOf is the driver's situation for record r, whose current
// execution the coordinator holds as rec, nil when it has not received it.
// The deadline is read on the coordinator's clock, the one by which it
// refuses to start work.
func (o *Owner) situationOf(r record, rec *session.Record) situation {
	s := situation{interactive: r.Intent.Mode == ModeInteractive,
		revoked: !project.Member(o.config.Projects, r.Association.Project, o.config.Principal)}
	switch {
	case rec == nil:
		s.phase, s.lateStart = phaseNotReceived, o.pastFirstDeadline(r)
	case rec.Phase.Starting():
		s.lateStart = o.config.Coordinator.PastDeadline(*rec)
	case rec.Phase == session.PhaseIdle:
		// The coordinator ends a start for its deadline only once the
		// deadline has passed, so before it, or with none, the placement
		// refused it. Past it, the refusal problem the driver recorded when
		// the coordinator reported it is the only record of which.
		ended, past := startEnded(*rec), o.config.Coordinator.PastDeadline(*rec)
		s.refused = ended && (!past || refusalProblem(r.Problem))
		s.lateStart = ended && !s.refused
	}
	if rec != nil {
		s.phase, s.cancel = rec.Phase, rec.CancelOperation != ""
	}
	return s
}

// pastFirstDeadline reports, on the coordinator's clock, that the deadline
// of r's first execution has passed, so the coordinator would refuse to
// create it.
func (o *Owner) pastFirstDeadline(r record) bool {
	return o.config.Coordinator.PastDeadline(session.Record{Deadline: r.First.Deadline})
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
	phases                              []session.Phase
	mode                                rowMode
	cancel, revoked, lateStart, refused condition
	action                              action
}

func (row decisionRow) matches(s situation) bool {
	return slices.Contains(row.phases, s.phase) && row.mode.holds(s.interactive) &&
		row.cancel.holds(s.cancel) && row.revoked.holds(s.revoked) && row.lateStart.holds(s.lateStart) &&
		row.refused.holds(s.refused)
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
// before its first visit started (ADR 0060). A placement's refusal of the
// first visit ends a task too, whatever the deadline, naming the refusal
// rather than the deadline (ADR 0067); an interactive session it ended stays
// stopped for its owner, as one the deadline ended does.
var decisions = []decisionRow{
	{phases: []session.Phase{session.PhaseCancelled}, action: actNothing},
	{phases: []session.Phase{session.PhaseIdle}, mode: interactiveOnly, action: actNothing},
	{phases: []session.Phase{session.PhaseIdle}, mode: taskOnly, lateStart: isTrue, refused: isFalse, action: actEndExpired},
	{phases: []session.Phase{session.PhaseIdle}, mode: taskOnly, refused: isTrue, action: actEndRefused},
	{phases: []session.Phase{session.PhaseIdle}, mode: taskOnly, lateStart: isFalse, refused: isFalse, action: actFinish},

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
