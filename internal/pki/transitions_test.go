package pki

import (
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// Every phase is reachable from an empty store, and from every phase every
// other phase is reachable, so no phase is a dead end and none waits on
// anything but an event. Guards transitions in transitions.go.
func TestEveryPhaseIsReachableAndHasAnExit(t *testing.T) {
	for ev, rows := range transitions {
		for from, to := range rows {
			if from == phaseNone && ev != evInitialize {
				t.Errorf("%s starts from no record", ev)
			}
			if from != phaseNone && !slices.Contains(Phases, from) || !slices.Contains(Phases, to) {
				t.Errorf("%s: %q -> %q is outside the vocabulary", ev, from, to)
			}
		}
	}
	reach := func(start Phase) map[Phase]bool {
		seen := map[Phase]bool{start: true}
		for queue := []Phase{start}; len(queue) > 0; queue = queue[1:] {
			for _, rows := range transitions {
				if to, ok := rows[queue[0]]; ok && !seen[to] {
					seen[to] = true
					queue = append(queue, to)
				}
			}
		}
		return seen
	}
	for _, start := range append([]Phase{phaseNone}, Phases...) {
		seen := reach(start)
		for _, p := range Phases {
			if !seen[p] {
				t.Errorf("%q cannot reach %q", start, p)
			}
		}
	}
}

// phaseStates are a store state in each phase.
var phaseStates = map[Phase]string{PhaseSingle: "single", PhasePrepared: "prepared", PhaseSwitched: "switched"}

// eventOperations runs each event through the store as its callers do.
var eventOperations = map[event]func(s *Store) error{
	evInitialize: func(s *Store) error { return s.Initialize(testNames) },
	evPrepare:    func(s *Store) error { return s.PrepareRotation() },
	evSwitch:     func(s *Store) error { return s.SwitchRotation(true) },
	evRollback:   func(s *Store) error { return s.RollbackRotation() },
	evRetire:     func(s *Store) error { return s.RetireRotation(true) },
	evReplace:    func(s *Store) error { return s.ReplaceCA() },
}

// Every declared move, run through the store, commits the phase the table
// declares; every undeclared one is refused by apply, which leaves the
// record as it was, and by the store, which writes nothing.
func TestTheStoreMovesOnlyByTheTable(t *testing.T) {
	roots := buildStates(t)
	for ev, op := range eventOperations {
		for _, from := range append([]Phase{phaseNone}, Phases...) {
			name := "empty"
			if from != phaseNone {
				name = phaseStates[from]
			}
			root := copyStore(t, filepath.Join(roots[name], "pki"))
			s := outcomeStore(t, root)
			before, _ := s.load()
			to, declared := transitions[ev][from]
			if !declared {
				st := before.clone()
				if err := st.apply(ev, &Generation{ID: len(st.Generations) + 1}, outcomeStart); err == nil {
					t.Errorf("apply %s from %q was accepted", ev, from)
				}
				if !reflect.DeepEqual(st, before) {
					t.Errorf("refused apply %s from %q changed the record", ev, from)
				}
			}
			startDescription := describeStore(root, leafOf(root))
			err := op(s)
			if !declared {
				if err == nil {
					t.Errorf("%s from %q was accepted", ev, from)
				}
				if got := describeStore(root, leafOf(root)); got != startDescription {
					t.Errorf("refused %s from %q changed the store:\n got %s\nwant %s", ev, from, got, startDescription)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s from %q: %v", ev, from, err)
				continue
			}
			after, err := s.State()
			if err != nil || after.Phase() != to {
				t.Errorf("%s from %q committed %q (%v), want %q", ev, from, after.Phase(), err, to)
			}
		}
	}
}
