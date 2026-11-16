package home

import (
	"fmt"
	"slices"

	"github.com/andrewmccall/sproozi/internal/session"
)

// Phase is a home session's lifecycle phase: the word "sproozi status" and
// "sproozi sessions" report for it. It is never stored. derive works it out
// from the session's catalogue entry and its current execution's journal
// record, so a catalogue entry left behind by an interrupted command reads as
// what the journal records (ADR 0058).
type Phase string

const (
	// PhaseImporting is a pull whose claim the session's source has not
	// acknowledged: the downloaded checkpoint is kept, but the session cannot
	// run here.
	PhaseImporting Phase = "importing"
	// PhaseRetained is a stopped session with its work kept: no current
	// execution, or one that is idle.
	PhaseRetained Phase = "retained"
	// PhaseFailed is a stopped session whose agent exited by itself with a
	// failure (session.Exit.Failed).
	PhaseFailed Phase = "failed"
)

// Ownership in a session's status: this machine's service, or, while a pull
// is unfinished, the session's source.
const (
	ownershipHome   = "home"
	ownershipSource = "source"
)

// phases is every phase a home session can have: the three above and every
// execution phase except idle, which a session reports as retained or failed.
var phases = []Phase{PhaseImporting, PhaseRetained, PhaseFailed,
	Phase(session.PhasePreparing), Phase(session.PhaseActivating), Phase(session.PhaseActive),
	Phase(session.PhaseStopping), Phase(session.PhaseUnknownStop), Phase(session.PhaseUnknownUsage),
	Phase(session.PhaseCancelling), Phase(session.PhaseCancelled)}

// state is what derive works out about a session: its phase and the facts
// that decide what a user can do next.
type state struct {
	phase Phase
	// exit is a stopped session's last exit, or nil.
	exit *session.Exit
	// cancelRequested: the current execution has cancellation intent that is
	// not yet carried out.
	cancelRequested bool
	// overdue: a start or move whose deadline passed before its destination
	// became active (ADR 0060).
	overdue bool
	// finalizing: the pending stop is a stop, not a move's.
	finalizing bool
}

// stopped reports a session with no execution in progress and its work kept.
func (st state) stopped() bool { return st.phase == PhaseRetained || st.phase == PhaseFailed }

// derive is the one place a home session's state is worked out. c is the
// session's catalogue entry, or nil for an execution no session records;
// current is c's current execution's journal record, or nil when c names
// none. pastDeadline reports that an execution's deadline has passed.
//
// An idle current execution is a stop the catalogue has not recorded yet,
// because the command was interrupted after the journal recorded it: the
// session reads as stopped with that stop, as it will once a command holding
// the session's lock retires it (retireSettled).
func derive(c *localSession, current *session.Record, pastDeadline func(session.Record) bool) state {
	switch {
	case c != nil && !c.Claimed:
		return state{phase: PhaseImporting}
	case current == nil && c == nil:
		return state{phase: PhaseRetained}
	case current == nil:
		return stoppedState(c.Committed.Exit)
	case current.Phase == session.PhaseIdle:
		// Idle never carries cancellation intent: cancelling an idle
		// execution records cancelled at once.
		var exit *session.Exit
		switch {
		case len(current.Visits) != 0:
			exit = current.Visits[len(current.Visits)-1].Exit
		case c != nil:
			exit = c.Committed.Exit
		}
		return stoppedState(exit)
	}
	st := state{phase: Phase(current.Phase), cancelRequested: current.CancelOperation != ""}
	// A start or move with cancellation intent is cancelling, never preparing
	// or activating, so overdue needs no check of it.
	st.overdue = current.Phase.Starting() && pastDeadline(*current)
	st.finalizing = current.PendingOperation != "" && current.Operations[len(current.Operations)-1].Finalize
	return st
}

func stoppedState(exit *session.Exit) state {
	if exit.Failed() {
		return state{phase: PhaseFailed, exit: exit}
	}
	return state{phase: PhaseRetained, exit: exit}
}

// step names a row of the next-steps table: what a user can do next, which
// depends on the phase and on the facts state adds to it.
type step string

const (
	stepImporting  step = "importing"
	stepRetained   step = "retained"
	stepFailed     step = "failed"
	stepStarting   step = "starting"
	stepOverdue    step = "overdue"
	stepRunning    step = "running"
	stepStopping   step = "stopping"
	stepMoving     step = "moving"
	stepCancelling step = "cancelling"
	stepCancelled  step = "cancelled"
)

func (st state) step() step {
	switch {
	case st.phase == PhaseImporting:
		return stepImporting
	case st.phase == PhaseRetained:
		return stepRetained
	case st.phase == PhaseFailed:
		return stepFailed
	case st.phase == Phase(session.PhaseCancelled):
		return stepCancelled
	case st.cancelRequested:
		return stepCancelling
	case st.overdue:
		return stepOverdue
	case session.Phase(st.phase).Starting():
		return stepStarting
	case st.phase == Phase(session.PhaseActive):
		return stepRunning
	case st.finalizing:
		return stepStopping
	}
	return stepMoving
}

// next is a row of the next-steps table.
type next struct {
	// refuses lists the commands the service refuses in this state, whether
	// the command names the session or its current execution. Every other
	// command is carried out: by the service itself where it acts on the
	// catalogue (pull; stop of a stopped session repeats the stop, resume of
	// one starts a new execution, rm removes it), or by the execution
	// journal, which may still refuse by its own transitions table
	// (internal/session/phase.go), in the words needsRecovery gives.
	refuses []string
	// problem says, after "session ID ", what state the session is in and
	// which commands work from it. It is the text of every refusal this
	// table makes.
	problem func(id string, st state) string
}

// nextSteps is the one table of what a user can do next from each state.
// Every refusal the service makes from a session's state, and the commands
// it names, come from here, so the advice and the refusals cannot disagree.
var nextSteps = map[step]next{
	stepImporting: {
		// rm refuses an unfinished pull (ADR 0037): if the source recorded a
		// claim whose reply was lost, only this entry can repeat it.
		refuses: []string{ActionResume, ActionAttach, ActionStop, ActionMove, ActionRecover, ActionCancel,
			ActionRemove},
		problem: func(id string, _ state) string {
			return fmt.Sprintf("is still being pulled; finish with \"sproozi pull %s\" first", id)
		},
	},
	stepRetained: {
		refuses: []string{ActionAttach, ActionMove, ActionRecover, ActionCancel},
		problem: func(id string, _ state) string {
			return fmt.Sprintf("is retained (stopped, with its work kept); \"sproozi resume %s\" continues it", id)
		},
	},
	stepFailed: {
		refuses: []string{ActionAttach, ActionMove, ActionRecover, ActionCancel},
		problem: func(id string, st state) string {
			return fmt.Sprintf("stopped because its agent exited with code %d; see \"sproozi status %s\", then "+
				"\"sproozi resume %s\" to continue it or \"sproozi rm %s\" to remove it", st.exit.Code, id, id, id)
		},
	},
	stepStarting: {
		refuses: []string{ActionResume, ActionAttach, ActionRemove},
		problem: func(id string, _ state) string {
			return fmt.Sprintf("is still starting or moving; run \"sproozi recover %s\" to finish that first, or "+
				"\"sproozi cancel %s\" to stop it now", id, id)
		},
	},
	stepOverdue: {
		refuses: []string{ActionAttach, ActionRemove},
		problem: func(id string, _ state) string {
			return fmt.Sprintf("reached its deadline before it started; \"sproozi resume %s\" ends that start and "+
				"continues the session with a new deadline, or, if the placement cannot show that nothing runs "+
				"there, \"sproozi cancel %s\" records the cancellation and finishes it once the placement can show "+
				"that", id, id)
		},
	},
	stepRunning: {
		refuses: []string{ActionResume, ActionRemove},
		problem: func(id string, _ state) string {
			return fmt.Sprintf("is running; attach to it with \"sproozi attach %s\", stop it with \"sproozi stop %s\", "+
				"or force-stop it with \"sproozi cancel %s\"", id, id, id)
		},
	},
	stepStopping: {
		refuses: []string{ActionResume, ActionAttach, ActionRemove},
		problem: func(id string, _ state) string {
			return fmt.Sprintf("is still stopping; run \"sproozi stop %s\" to finish, or \"sproozi cancel %s\" to "+
				"force-stop it", id, id)
		},
	},
	stepMoving: {
		refuses: []string{ActionResume, ActionAttach, ActionRemove},
		problem: func(id string, _ state) string {
			return fmt.Sprintf("is still stopping to move; run \"sproozi recover %s\" to finish the move, or "+
				"\"sproozi cancel %s\" to force-stop it", id, id)
		},
	},
	stepCancelling: {
		refuses: []string{ActionResume, ActionAttach, ActionRemove},
		problem: func(id string, _ state) string {
			return fmt.Sprintf("is being cancelled; run \"sproozi recover %s\" to finish the cancellation, then "+
				"\"sproozi rm %s\" to remove it", id, id)
		},
	},
	stepCancelled: {
		refuses: []string{ActionResume, ActionAttach, ActionStop},
		problem: func(id string, _ state) string {
			return fmt.Sprintf("was cancelled, so it cannot be stopped or resumed; remove it with \"sproozi rm %s\", "+
				"and start new work with \"sproozi run\"", id)
		},
	},
}

// problem is the refusal of a command in st, naming what works instead.
func problem(id string, st state) error {
	return rejectedError("home: session " + id + " " + nextSteps[st.step()].problem(id, st))
}

// refusal is problem when st refuses action, otherwise nil.
func refusal(id string, st state, action string) error {
	if slices.Contains(nextSteps[st.step()].refuses, action) {
		return problem(id, st)
	}
	return nil
}

// refuseIn is refusal of action in c's derived state.
func (s *Service) refuseIn(c localSession, action string) error {
	st, _, err := s.sessionState(c)
	if err != nil {
		return err
	}
	return refusal(c.ID, st, action)
}

// sessionState derives c's state from its current execution's journal
// record, which it also returns, nil when c names none.
func (s *Service) sessionState(c localSession) (state, *session.Record, error) {
	var current *session.Record
	if c.Claimed && c.Current != "" {
		r, err := s.coordinator.Inspect(c.Current)
		if err != nil {
			return state{}, nil, err
		}
		current = &r
	}
	return derive(&c, current, s.coordinator.PastDeadline), current, nil
}
