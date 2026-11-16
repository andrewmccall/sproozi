package main

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
)

// starts are the phases serve starts the supervisor in.
var starts = []phase{phaseUnconfigured, phasePrepared}

// reachable is every phase reachable from starts through events allowed by
// take.
func reachable(from []phase, take func(event, rule) bool) map[phase]bool {
	reached := map[phase]bool{}
	for _, p := range from {
		reached[p] = true
	}
	for changed := true; changed; {
		changed = false
		for e, r := range transitions {
			if !take(e, r) {
				continue
			}
			for f, to := range r.to {
				if reached[f] && !reached[to] {
					reached[to], changed = true, true
				}
			}
		}
	}
	return reached
}

// Every phase is reachable from a start, the table names no other phase, and
// ended is the only phase with no way on.
func TestLifecycleTableIsClosed(t *testing.T) {
	for e, r := range transitions {
		for from, to := range r.to {
			if !slices.Contains(phases, from) || !slices.Contains(phases, to) {
				t.Fatalf("%s: unknown phase in %q -> %q", e, from, to)
			}
		}
	}
	reached := reachable(starts, func(event, rule) bool { return true })
	for _, p := range phases {
		if !reached[p] {
			t.Errorf("phase %s is unreachable", p)
		}
		leaves := false
		for _, r := range transitions {
			if to, ok := r.to[p]; ok && to != p {
				leaves = true
			}
		}
		if leaves == (p == phaseEnded) {
			t.Errorf("phase %s: has a way on = %v", p, leaves)
		}
	}
}

// No phase waits on the deadline to end (ADR 0060). Every phase but ended
// has an event the deadline never refuses that leads to another phase, and
// reaches ended through such events without the deadline event, so a
// supervisor with no deadline ends too.
func TestEveryPhaseEndsWithoutTheDeadline(t *testing.T) {
	for _, p := range phases {
		if p == phaseEnded {
			continue
		}
		exit := false
		for e, r := range transitions {
			if to, ok := r.to[p]; ok && to != p && !r.refusedAfterDeadline && e != evDeadline {
				exit = true
			}
		}
		if !exit {
			t.Errorf("phase %s has no exit that works with no deadline", p)
		}
		reached := reachable([]phase{p}, func(e event, r rule) bool {
			return !r.refusedAfterDeadline && e != evDeadline
		})
		if !reached[phaseEnded] {
			t.Errorf("phase %s cannot end without the deadline", p)
		}
	}
}

// The deadline refuses only the events that start work. Stopping, quiescing
// and ending are never refused (ADR 0058, rule 5).
func TestDeadlineRefusesOnlyWhatStartsWork(t *testing.T) {
	for e, r := range transitions {
		if r.refusedAfterDeadline != (e == evConfigure || e == evActivate) {
			t.Errorf("%s: refused after the deadline = %v", e, r.refusedAfterDeadline)
		}
	}
	l := lifecycle{phase: phasePrepared}
	if err := l.admit(evActivate, true); !errors.Is(err, errPastDeadline) {
		t.Fatalf("activate after the deadline: %v", err)
	}
	if err := l.admit(evActivate, false); err != nil {
		t.Fatalf("activate before the deadline: %v", err)
	}
	for _, p := range phases {
		for _, e := range []event{evGate, evFinish, evFinished, evTerminate, evDeadline, evFail, evExited} {
			l := lifecycle{phase: p}
			if errors.Is(l.admit(e, true), errPastDeadline) {
				t.Errorf("%s in %s refused after the deadline", e, p)
			}
		}
	}
}

// apply refuses a change the table does not declare and leaves the
// lifecycle unchanged.
func TestApplyRefusesUndeclaredChange(t *testing.T) {
	for _, tc := range []struct {
		from phase
		e    event
	}{
		{phasePrepared, evGate}, {phaseUnconfigured, evActivate}, {phaseActive, evConfigure},
		{phaseEnded, evActivate}, {phaseEnded, evGate}, {phaseActive, evFinish}, {phasePrepared, evExited},
		{phaseGated, evFinished}, {phaseForcing, evGate},
	} {
		l := lifecycle{phase: tc.from, gatedAt: time.Unix(1, 0)}
		before := l
		if l.apply(change{event: tc.e, at: time.Now()}) || l != before {
			t.Errorf("%s in %s: applied, now %+v", tc.e, tc.from, l)
		}
		if !errors.Is(before.admit(tc.e, false), errUndeclared) {
			t.Errorf("%s in %s admitted", tc.e, tc.from)
		}
	}
}

// The cause is decided once, by the event that forces or settles the end,
// and the supervisor never exits 0 after a forced stop.
func TestCauseIsDecidedOnce(t *testing.T) {
	killed := nativepty.Exit{Code: -1, Reason: nativepty.ForcedStop}
	crashed := nativepty.Exit{Code: 3, Reason: nativepty.NativeExit}
	for _, tc := range []struct {
		name   string
		from   phase
		events []change
		cause  exitCause
		codes  [2]int
	}{
		{"own exit", phaseActive, []change{{event: evExited, exit: nativepty.Exit{Code: 3}}}, causeSelf, [2]int{3, 3}},
		{"own exit, group stop unconfirmed", phaseGated,
			[]change{{event: evExited, err: errors.New("unconfirmed")}}, causeSelf, [2]int{0, 1}},
		{"SIGTERM", phaseActive, []change{{event: evTerminate}, {event: evExited, exit: killed}},
			causeTerminated, [2]int{-1, -1}},
		{"deadline before launch", phasePrepared, []change{{event: evDeadline}, {event: evExited}},
			causeDeadline, [2]int{0, 1}},
		{"graceful", phaseGated, []change{{event: evFinish}, {event: evFinished, exit: killed}},
			causeQuiesced, [2]int{-1, 0}},
		{"terminated while finishing", phaseFinishing,
			[]change{{event: evTerminate}, {event: evExited, exit: killed}, {event: evFinished, exit: killed}},
			causeTerminated, [2]int{-1, -1}},
		{"graceful end before the forced stop's", phaseFinishing,
			[]change{{event: evDeadline}, {event: evFinished, exit: killed}, {event: evExited, exit: killed}},
			causeDeadline, [2]int{-1, 0}},
		{"fail after a terminate", phaseFinishing, []change{{event: evTerminate}, {event: evFail}},
			causeTerminated, [2]int{0, 0}},
		{"exit while finishing", phaseFinishing,
			[]change{{event: evExited, exit: crashed}, {event: evFinished, exit: crashed, code: 3}},
			causeQuiesced, [2]int{3, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := lifecycle{phase: tc.from}
			for _, c := range tc.events {
				if !l.apply(c) {
					t.Fatalf("%s refused in %s", c.event, l.phase)
				}
			}
			if l.cause != tc.cause || [2]int{l.harnessCode, l.exitCode} != tc.codes {
				t.Fatalf("cause %d codes %d %d, want %d %v", l.cause, l.harnessCode, l.exitCode, tc.cause, tc.codes)
			}
		})
	}
}

// Status reports the control protocol's states: a gated or finishing
// supervisor is stopping, a forcing or ended one stopped.
func TestPhasesReportTheProtocolStates(t *testing.T) {
	want := map[phase]harness.NativeState{
		phaseUnconfigured: harness.NativeUnconfigured, phasePrepared: harness.NativePrepared,
		phaseActive: harness.NativeActive, phaseGated: harness.NativeStopping, phaseFinishing: harness.NativeStopping,
		phaseForcing: harness.NativeStopped, phaseEnded: harness.NativeStopped,
	}
	for _, p := range phases {
		if p.wire() != want[p] {
			t.Errorf("%s reports %s, want %s", p, p.wire(), want[p])
		}
	}
}
