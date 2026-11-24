package docker

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
)

// Every declared transition names known phases, every phase is reachable from
// no record, and only revoked has no way on.
func TestGrantTransitionTableIsClosed(t *testing.T) {
	reached := map[grantPhase]bool{grantNone: true}
	for changed := true; changed; {
		changed = false
		for ev, rule := range grantTransitions {
			for from, to := range rule.to {
				if from != grantNone && !slices.Contains(grantPhases, from) || !slices.Contains(grantPhases, to) {
					t.Fatalf("%s: unknown phase in %q -> %q", ev, from, to)
				}
				if reached[from] && !reached[to] {
					reached[to], changed = true, true
				}
			}
		}
	}
	for _, p := range grantPhases {
		if !reached[p] {
			t.Errorf("phase %s is unreachable", p)
		}
		leaves := false
		for _, rule := range grantTransitions {
			if to, ok := rule.to[p]; ok && to != p {
				leaves = true
			}
		}
		if leaves == (p == revoked) {
			t.Errorf("phase %s: has a way on = %v", p, leaves)
		}
	}
}

// apply refuses every transition the table does not declare and leaves the
// record unchanged.
func TestGrantApplyRefusesUndeclaredTransitions(t *testing.T) {
	for ev, rule := range grantTransitions {
		for _, from := range append([]grantPhase{grantNone}, grantPhases...) {
			r := record{Phase: from}
			err := r.apply(ev)
			want, declared := rule.to[from]
			switch {
			case declared && (err != nil || r.Phase != want):
				t.Errorf("%s from %q: %v, phase %s, want %s", ev, from, err, r.Phase, want)
			case !declared && (err == nil || r.Phase != from || !strings.Contains(err.Error(), "refused")):
				t.Errorf("undeclared %s from %q: %v, phase now %s", ev, from, err, r.Phase)
			}
		}
	}
}

// Events that start work are refused once the launch deadline has passed;
// revoking, sealing and binding are not. No deadline refuses nothing.
func TestGrantDeadlineRefusesOnlyEventsThatStartWork(t *testing.T) {
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	starting := []grantEvent{evInitialize, evInitialized, evActivate}
	for ev, rule := range grantTransitions {
		if rule.refusedAfterDeadline != slices.Contains(starting, ev) {
			t.Errorf("%s: refusedAfterDeadline = %v", ev, rule.refusedAfterDeadline)
		}
		for from := range rule.to {
			r := record{Phase: from, Launch: session.Launch{Deadline: at}}
			if err := r.admit(ev, at); (err != nil) != rule.refusedAfterDeadline {
				t.Errorf("%s from %q past the deadline: %v", ev, from, err)
			}
			if err := r.admit(ev, at.Add(-time.Second)); err != nil {
				t.Errorf("%s from %q before the deadline: %v", ev, from, err)
			}
			r.Launch.Deadline = time.Time{}
			if err := r.admit(ev, at.Add(100*365*24*time.Hour)); err != nil {
				t.Errorf("%s from %q without a deadline: %v", ev, from, err)
			}
		}
	}
	if err := (record{Phase: revoked}).admit(evActivate, at); err == nil {
		t.Fatal("admitted an undeclared event")
	}
}

// No phase waits on the deadline to end (ADR 0060). Terminal means revoked;
// an exit is an event that admit accepts past the launch's deadline and that
// leads to a different phase. Every other phase has one. The grant's other
// deadline check, refusing to authenticate after it, records no event.
func TestEveryGrantPhaseHasAnExitAfterTheDeadline(t *testing.T) {
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	for _, p := range grantPhases {
		if p == revoked {
			continue
		}
		r := record{Phase: p, Launch: session.Launch{Deadline: at}}
		exit := false
		for ev, rule := range grantTransitions {
			if to, ok := rule.to[p]; ok && to != p && r.admit(ev, at) == nil {
				exit = true
			}
		}
		if !exit {
			t.Errorf("phase %s has no exit after the deadline", p)
		}
	}
}
