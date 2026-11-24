package registry

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// Every declared transition names known phases, every phase is reachable from
// no registration, and exactly stopped and cancelled have no way on.
func TestRegistrationTransitionTableIsClosed(t *testing.T) {
	reached := map[Phase]bool{phaseNone: true}
	for changed := true; changed; {
		changed = false
		for ev, rule := range registrationTransitions {
			for from, to := range rule.to {
				if from != phaseNone && !slices.Contains(phases, from) || !slices.Contains(phases, to) {
					t.Fatalf("%s: unknown phase in %q -> %q", ev, from, to)
				}
				if reached[from] && !reached[to] {
					reached[to], changed = true, true
				}
			}
		}
	}
	terminal := []Phase{Stopped, Cancelled}
	for _, p := range phases {
		if !reached[p] {
			t.Errorf("phase %s is unreachable", p)
		}
		leaves := false
		for _, rule := range registrationTransitions {
			if to, ok := rule.to[p]; ok && to != p {
				leaves = true
			}
		}
		if leaves == slices.Contains(terminal, p) {
			t.Errorf("phase %s: has a way on = %v", p, leaves)
		}
	}
}

// apply refuses every transition the table does not declare, as a conflict,
// and leaves the registration unchanged.
func TestRegistrationApplyRefusesUndeclaredTransitions(t *testing.T) {
	for ev, rule := range registrationTransitions {
		for _, from := range append([]Phase{phaseNone}, phases...) {
			r := stored{Phase: from}
			err := r.apply(ev)
			want, declared := rule.to[from]
			switch {
			case declared && (err != nil || r.Phase != want):
				t.Errorf("%s from %q: %v, phase %s, want %s", ev, from, err, r.Phase, want)
			case !declared && (!errors.Is(err, ErrConflict) || r.Phase != from):
				t.Errorf("undeclared %s from %q: %v, phase now %s", ev, from, err, r.Phase)
			}
			r = stored{Phase: from}
			if err = r.admit(ev, time.Time{}); (err == nil) != declared {
				t.Errorf("admit %s from %q: %v", ev, from, err)
			}
		}
	}
}

// Only create and activate, which start work, are refused once the
// registration's deadline has passed; recording an observed outcome,
// stopping, cancelling and accounting are not. A registration without a
// deadline refuses nothing.
func TestRegistrationDeadlineRefusesOnlyEventsThatStartWork(t *testing.T) {
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	starting := []registrationEvent{evCreate, evActivate}
	for ev, rule := range registrationTransitions {
		if rule.refusedAfterDeadline != slices.Contains(starting, ev) {
			t.Errorf("%s: refusedAfterDeadline = %v", ev, rule.refusedAfterDeadline)
		}
		for from := range rule.to {
			r := stored{Phase: from, Registration: storedRegistration{Registration: Registration{OriginalDeadline: at}}}
			if err := r.admit(ev, at); (err != nil) != rule.refusedAfterDeadline || err != nil && !errors.Is(err, ErrExpired) {
				t.Errorf("%s from %q past the deadline: %v", ev, from, err)
			}
			if err := r.admit(ev, at.Add(-time.Second)); err != nil {
				t.Errorf("%s from %q before the deadline: %v", ev, from, err)
			}
			r.Registration.OriginalDeadline = time.Time{}
			if err := r.admit(ev, at.Add(100*365*24*time.Hour)); err != nil {
				t.Errorf("%s from %q without a deadline: %v", ev, from, err)
			}
		}
	}
	if err := (stored{Phase: Stopped}).admit(evActivate, at); !errors.Is(err, ErrConflict) {
		t.Fatalf("admitted an undeclared event: %v", err)
	}
}

// No phase waits on the deadline to end (ADR 0060). Terminal means stopped
// and cancelled; an exit is an event that admit accepts past the
// registration's deadline and that leads to a different phase, cancel
// included: the session keeps its checkpoint in the execution journal, not
// here. Every other phase has one.
func TestEveryRegistrationPhaseHasAnExitAfterTheDeadline(t *testing.T) {
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	for _, p := range phases {
		if p == Stopped || p == Cancelled {
			continue
		}
		r := stored{Phase: p, Registration: storedRegistration{Registration: Registration{OriginalDeadline: at}}}
		exit := false
		for ev, rule := range registrationTransitions {
			if to, ok := rule.to[p]; ok && to != p && r.admit(ev, at) == nil {
				exit = true
			}
		}
		if !exit {
			t.Errorf("phase %s has no exit after the deadline", p)
		}
	}
}
