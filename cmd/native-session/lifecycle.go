package main

import (
	"errors"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
)

// phase is where the supervisor's lifecycle stands (ADR 0058, rules 1, 3, 4
// and 5). Only apply, following the transitions table, changes it. Status
// reports it in the control protocol's coarser words (wire).
type phase string

const (
	// phaseUnconfigured: a bootstrapped supervisor waits for its launch
	// contract.
	phaseUnconfigured phase = "unconfigured"
	// phasePrepared: configured; the harness has not started.
	phasePrepared phase = "prepared"
	// phaseActive: the harness runs and takes terminal input.
	phaseActive phase = "active"
	// phaseGated: a stop has begun and input is gated. The harness still
	// runs, and an exit it makes is its own: the exit watcher settles it.
	phaseGated phase = "gated"
	// phaseFinishing: a graceful stop owns the harness's exit. It quits or
	// kills a harness whose turn completed, or a turnless first visit, and
	// settles the exit itself.
	phaseFinishing phase = "finishing"
	// phaseForcing: the supervisor is killing the harness for a decided
	// cause (terminated, deadline or failed). Whichever sees the harness's
	// exit first, the stop or the exit watcher, settles it.
	phaseForcing phase = "forcing"
	// phaseEnded: the harness's exit is settled, with its cause and codes.
	// The supervisor concludes from here and nothing leaves it.
	phaseEnded phase = "ended"
)

// phases is every phase, in lifecycle order.
var phases = []phase{phaseUnconfigured, phasePrepared, phaseActive, phaseGated, phaseFinishing, phaseForcing,
	phaseEnded}

// wire is the state status and every control reply report.
func (p phase) wire() harness.NativeState {
	switch p {
	case phaseUnconfigured:
		return harness.NativeUnconfigured
	case phasePrepared:
		return harness.NativePrepared
	case phaseActive:
		return harness.NativeActive
	case phaseGated, phaseFinishing:
		return harness.NativeStopping
	}
	return harness.NativeStopped
}

// takesInput: terminal input and new attachments reach the harness.
func (p phase) takesInput() bool { return p == phaseActive }

// running: the harness was launched and its end is not yet being forced or
// settled, so its conversation and lifecycle hooks are still observed.
func (p phase) running() bool { return p == phaseActive || p.stopping() }

// stopping: input is gated for a stop that has not ended the harness.
func (p phase) stopping() bool { return p == phaseGated || p == phaseFinishing }

// over: the harness is being killed or has ended.
func (p phase) over() bool { return p == phaseForcing || p == phaseEnded }

// event is something that changes the supervisor's lifecycle: a control
// request, or an outcome of the harness or of the supervisor's own stop.
type event string

const (
	// evConfigure: configure accepted the launch contract.
	evConfigure event = "configure"
	// evActivate: activate launched the harness.
	evActivate event = "activate"
	// evGate: gate or quiesce gated terminal input.
	evGate event = "gate"
	// evTurnless: the observer found that a gated first visit has no
	// conversation to keep. It is latched.
	evTurnless event = "turnless"
	// evFinish: a graceful stop takes over the harness's exit.
	evFinish event = "finish"
	// evFinished: the graceful stop's outcome, the harness ended.
	evFinished event = "finished"
	// evTerminate: the supervisor was told to stop (SIGTERM or SIGINT).
	evTerminate event = "terminate"
	// evDeadline: the execution's deadline passed (deadline.go raises it).
	evDeadline event = "deadline"
	// evFail: a graceful stop could not finish.
	evFail event = "fail"
	// evExited: the harness's exit was seen, by the exit watcher or by the
	// stop that killed it.
	evExited event = "exited"
)

// rule is one event's row: the phases it may happen in, the phase each
// leads to, and whether a passed deadline refuses it (rule 5).
type rule struct {
	refusedAfterDeadline bool
	to                   map[phase]phase
}

// forcedFrom is where terminate, deadline and fail lead: every phase before
// the end is forced, and a cause decided earlier stays.
var forcedFrom = map[phase]phase{
	phaseUnconfigured: phaseForcing, phasePrepared: phaseForcing, phaseActive: phaseForcing,
	phaseGated: phaseForcing, phaseFinishing: phaseForcing, phaseForcing: phaseForcing, phaseEnded: phaseEnded,
}

// transitions declares every lifecycle change. Only configure and activate
// start work, so only they are refused after the deadline; stopping,
// quiescing and ending never are.
var transitions = map[event]rule{
	evConfigure: {refusedAfterDeadline: true, to: map[phase]phase{phaseUnconfigured: phasePrepared}},
	evActivate:  {refusedAfterDeadline: true, to: map[phase]phase{phasePrepared: phaseActive}},
	evGate: {to: map[phase]phase{
		phaseActive: phaseGated, phaseGated: phaseGated, phaseFinishing: phaseFinishing}},
	evTurnless: {to: map[phase]phase{
		phaseGated: phaseGated, phaseFinishing: phaseFinishing, phaseForcing: phaseForcing, phaseEnded: phaseEnded}},
	evFinish: {to: map[phase]phase{
		phaseGated: phaseFinishing, phaseForcing: phaseForcing, phaseEnded: phaseEnded}},
	evFinished: {to: map[phase]phase{
		phaseFinishing: phaseEnded, phaseForcing: phaseEnded, phaseEnded: phaseEnded}},
	evTerminate: {to: forcedFrom},
	evDeadline:  {to: forcedFrom},
	evFail:      {to: forcedFrom},
	evExited: {to: map[phase]phase{
		phaseActive: phaseEnded, phaseGated: phaseEnded, phaseFinishing: phaseFinishing,
		phaseForcing: phaseEnded, phaseEnded: phaseEnded}},
}

// lifecycle is the supervisor's one lifecycle state: its phase and the facts
// that belong to it. The cause is decided on entering forcing or ended, and
// the codes on entering ended.
type lifecycle struct {
	phase phase
	cause exitCause
	// gatedAt is when input was first gated.
	gatedAt time.Time
	// turnless latches that this first visit has no conversation to keep.
	turnless bool
	// harnessCode is the harness's exit code, exitCode the supervisor's own.
	harnessCode, exitCode int
}

// change is an event with what it carries: when input was gated, and how
// the harness ended with the supervisor's code for a graceful end.
type change struct {
	event event
	at    time.Time
	exit  nativepty.Exit
	err   error
	code  int
}

// causeOf is the cause a forced stop decides.
func causeOf(e event) exitCause {
	switch e {
	case evTerminate:
		return causeTerminated
	case evDeadline:
		return causeDeadline
	case evFail:
		return causeFailed
	}
	return causeNone
}

var (
	errUndeclared   = errors.New("the lifecycle declares no such change")
	errPastDeadline = errors.New("the execution's deadline has passed")
)

// forcedCodes settles the codes of an exit nobody stopped gracefully: the
// supervisor exits with the harness's code, but never 0 after a forced stop
// or an unconfirmed process-group stop.
func (l *lifecycle) forcedCodes(code int, waitErr error) {
	l.harnessCode, l.exitCode = code, code
	if code == 0 && (waitErr != nil || l.cause.forced()) {
		l.exitCode = 1
	}
}

// admit reports whether e may happen now, before its side effect: declared
// from the current phase, and not refused by a deadline that has passed.
func (l lifecycle) admit(e event, pastDeadline bool) error {
	r := transitions[e]
	if _, ok := r.to[l.phase]; !ok {
		return errUndeclared
	}
	if r.refusedAfterDeadline && pastDeadline {
		return errPastDeadline
	}
	return nil
}

// apply moves the lifecycle by the table. An undeclared change is refused
// and leaves it unchanged. The caller holds the supervisor's mutex.
func (l *lifecycle) apply(c change) bool {
	to, ok := transitions[c.event].to[l.phase]
	if !ok {
		return false
	}
	from := l.phase
	switch c.event {
	case evGate:
		if l.gatedAt.IsZero() {
			l.gatedAt = c.at
		}
	case evTurnless:
		l.turnless = true
	case evTerminate, evDeadline, evFail:
		if !from.over() {
			l.cause = causeOf(c.event)
		}
	case evExited:
		if to == phaseEnded && from != phaseEnded {
			if from != phaseForcing {
				l.cause = causeSelf
			}
			l.forcedCodes(c.exit.Code, c.err)
		}
	case evFinished:
		// A cause decided earlier, such as a SIGTERM that raced the stop,
		// stays, and with it the forced stop's rule for the codes; otherwise
		// the graceful stop's codes stand, unless the exit was already
		// settled.
		switch from {
		case phaseFinishing:
			l.cause = causeQuiesced
			l.harnessCode, l.exitCode = c.exit.Code, c.code
		case phaseForcing:
			l.forcedCodes(c.exit.Code, c.err)
		}
	}
	l.phase = to
	return true
}
