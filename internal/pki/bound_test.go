package pki

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// padNames grows st's serving names until the record is exactly target
// bytes. The names stay sorted and valid, so Ensure reads them as the same
// list and a leaf can be issued for them.
func padNames(t *testing.T, st *State, target int) {
	t.Helper()
	label := strings.Repeat("a", 60)
	long := label + "." + label + "." + label + "." + label
	for i := 0; ; i++ {
		size, err := recordSize(*st)
		must(t, err)
		if size > target {
			t.Fatalf("record is %d bytes, beyond the target %d", size, target)
		}
		if target-size <= 240 {
			break
		}
		st.Names.DNS = append(st.Names.DNS, fmt.Sprintf("z%04d.%s", i, long)[:200])
	}
	// One last name, sized to land exactly on target.
	st.Names.DNS = append(st.Names.DNS, "z9999")
	size, err := recordSize(*st)
	must(t, err)
	missing := target - size
	if missing < 0 {
		t.Fatalf("record is %d bytes, beyond the target %d", size, target)
	}
	var name strings.Builder
	name.WriteString("z9999")
	for missing > 0 {
		n := min(missing, 60)
		name.WriteString("." + strings.Repeat("b", n-1))
		missing -= n
	}
	st.Names.DNS[len(st.Names.DNS)-1] = name.String()
	if size, _ = recordSize(*st); size != target {
		t.Fatalf("padded to %d bytes, want %d", size, target)
	}
}

// ninthGeneration rewrites a single store so its serving authority is
// generation 9 behind eight retired ones: a rotation's new generation, 10,
// is one digit longer.
func ninthGeneration(t *testing.T, dir string) {
	t.Helper()
	editState(func(st *State) {
		live := st.Generations[0]
		st.Generations = nil
		for id := 1; id <= 8; id++ {
			g := live
			g.ID, g.Retired, g.Fingerprint = id, true, strings.Repeat("ab", 32)
			st.Generations = append(st.Generations, g)
		}
		live.ID = 9
		st.Generations = append(st.Generations, live)
		st.Serving, st.Trusted = 9, []int{9}
	})(t, dir)
	for _, ext := range caFileExts {
		must(t, os.Rename(filepath.Join(dir, "ca-1"+ext), filepath.Join(dir, "ca-9"+ext)))
	}
}

func storedSize(t *testing.T, dir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, stateFile))
	must(t, err)
	var st State
	must(t, json.Unmarshal(data, &st))
	size, err := recordSize(st)
	must(t, err)
	return size
}

// boundStore copies base into a fresh store whose record is padded to size.
func boundStore(t *testing.T, base string, size int) *Store {
	t.Helper()
	root := copyStore(t, base)
	editState(func(st *State) { padNames(t, st, size) })(t, filepath.Join(root, "pki"))
	s := outcomeStore(t, root)
	// A timestamp with full nanosecond precision, the longest PreparedAt.
	s.now = func() time.Time { return outcomeStart.Add(987654321) }
	return s
}

// largestAccepted is the largest record size, padded from base, at which
// start succeeds.
func largestAccepted(t *testing.T, base string, start func(s *Store) error) int {
	t.Helper()
	lo, hi := storedSize(t, base), maxState
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if start(boundStore(t, base, mid)) == nil {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// boundBases builds a store in each phase, serving generation 9 so the
// rotation's generation 10 is a digit longer, with the clock at a
// timestamp of full nanosecond precision.
func boundBases(t *testing.T) map[Phase]string {
	t.Helper()
	roots := buildStates(t)
	root := copyStore(t, filepath.Join(roots[stSingle], "pki"))
	ninthGeneration(t, filepath.Join(root, "pki"))
	bases := map[Phase]string{PhaseSingle: filepath.Join(root, "pki")}
	prepared := copyStore(t, bases[PhaseSingle])
	s := outcomeStore(t, prepared)
	s.now = func() time.Time { return outcomeStart.Add(123456789) }
	must(t, s.PrepareRotation())
	bases[PhasePrepared] = filepath.Join(prepared, "pki")
	switched := copyStore(t, bases[PhasePrepared])
	must(t, outcomeStore(t, switched).SwitchRotation(true))
	bases[PhaseSwitched] = filepath.Join(switched, "pki")
	return bases
}

// caFileExts are a CA generation's two files.
var caFileExts = []string{".pem", ".key"}

type move struct {
	name string
	run  func(s *Store) error
}

var (
	movePrepare  = move{string(evPrepare), func(s *Store) error { return s.PrepareRotation() }}
	moveReplace  = move{string(evReplace), func(s *Store) error { return s.ReplaceCA() }}
	moveSwitch   = move{string(evSwitch), func(s *Store) error { return s.SwitchRotation(true) }}
	moveRollback = move{string(evRollback), func(s *Store) error { return s.RollbackRotation() }}
	moveRetire   = move{string(evRetire), func(s *Store) error { return s.RetireRotation(true) }}
	moveRenew    = move{"renew the leaf", func(s *Store) error { return s.Renew() }}
	moveEnsure   = move{"ensure with unchanged names", func(s *Store) error {
		st, err := s.State()
		if err != nil {
			return err
		}
		_, err = s.Ensure(st.Names)
		return err
	}}
)

// The size invariant: at the largest record a move that starts something
// (prepare, replace) accepts, from every phase, that move and every move
// that finishes or undoes what it started (switch, rollback, retire,
// renewal with unchanged names) commit, until the store is single again.
// Guards growthReserve and its check in startsWithin.
func TestEveryFinishingMoveCommitsAtTheLargestStartedRecord(t *testing.T) {
	bases := boundBases(t)
	chains := []struct {
		from  Phase
		start move
		then  []move
	}{
		{PhaseSingle, movePrepare, []move{moveSwitch, moveRollback, moveSwitch, moveRetire, moveRenew, moveEnsure}},
		{PhaseSingle, moveReplace, []move{moveRenew, moveEnsure}},
		{PhasePrepared, moveReplace, []move{moveRenew, moveEnsure}},
		{PhaseSwitched, moveReplace, []move{moveRenew, moveEnsure}},
	}
	for _, c := range chains {
		t.Run(fmt.Sprintf("%s from %s", c.start.name, c.from), func(t *testing.T) {
			size := largestAccepted(t, bases[c.from], c.start.run)
			s := boundStore(t, bases[c.from], size)
			for _, m := range append([]move{c.start}, c.then...) {
				if err := m.run(s); err != nil {
					t.Fatalf("at %d bytes, %s after %s: %v", size, m.name, c.start.name, err)
				}
			}
		})
	}
	// A record in a rotation phase at that same largest size finishes or
	// undoes the rotation.
	for _, c := range []struct {
		from Phase
		then []move
	}{
		{PhasePrepared, []move{moveRenew, moveSwitch, moveRollback, moveSwitch, moveRetire, moveRenew}},
		{PhaseSwitched, []move{moveRenew, moveRollback, moveSwitch, moveRetire, moveRenew}},
	} {
		t.Run(fmt.Sprintf("finishing from %s", c.from), func(t *testing.T) {
			size := largestAccepted(t, bases[c.from], moveReplace.run)
			s := boundStore(t, bases[c.from], size)
			for _, m := range c.then {
				if err := m.run(s); err != nil {
					t.Fatalf("at %d bytes, %s: %v", size, m.name, err)
				}
			}
		})
	}
}

// A names change that would leave the record without the headroom a
// finishing move needs is refused before the leaf is issued, so nothing is
// written, and the error names the names list; renewal with unchanged names
// keeps working at the same size. Guards the names check in Ensure.
func TestANamesChangeAtTheBoundIsRefusedFirst(t *testing.T) {
	bases := boundBases(t)
	s := boundStore(t, bases[PhaseSingle], maxState-5)
	st, err := s.State()
	must(t, err)
	before := storeFiles(t, s.Dir())
	wider := st.Names
	wider.DNS = append(append([]string(nil), wider.DNS...), "z9999z.example.com")
	_, err = s.Ensure(wider)
	if err == nil || !strings.Contains(err.Error(), "serving names") {
		t.Errorf("ensure with more names: %v, want a refusal naming the serving names", err)
	}
	if after := storeFiles(t, s.Dir()); !equalFiles(before, after) {
		t.Error("a refused names change wrote the store")
	}
	if err = s.Renew(); err != nil {
		t.Errorf("renew with unchanged names: %v", err)
	}
}

// The reserve's own boundary: prepare and replace start at exactly
// maxState - growthReserve bytes, and are refused one byte above, writing
// nothing. Guards the comparison in startsWithin (reviewer's R09).
func TestTheReserveBoundaryIsExact(t *testing.T) {
	bases := boundBases(t)
	for _, m := range []move{movePrepare, moveReplace} {
		if err := m.run(boundStore(t, bases[PhaseSingle], maxState-growthReserve)); err != nil {
			t.Errorf("%s at %d bytes: %v", m.name, maxState-growthReserve, err)
		}
		s := boundStore(t, bases[PhaseSingle], maxState-growthReserve+1)
		before := storeFiles(t, s.Dir())
		if err := m.run(s); err == nil || !strings.Contains(err.Error(), "refusing to "+m.name) {
			t.Errorf("%s one byte above the reserve: %v", m.name, err)
		}
		if after := storeFiles(t, s.Dir()); !equalFiles(before, after) {
			t.Errorf("a refused %s wrote the store", m.name)
		}
	}
}

// The same boundary for replace of a refused record: it starts when the
// history it rebuilds is exactly maxState - growthReserve bytes, and one
// byte more is refused naming the file it would number after. Guards the
// comparison in replaceable.
func TestTheReserveBoundaryOfARefusedReplaceIsExact(t *testing.T) {
	bases := boundBases(t)
	for _, extra := range []int{0, 1} {
		root := copyStore(t, bases[PhaseSingle])
		dir := filepath.Join(root, "pki")
		s := outcomeStore(t, root)
		// Refuse the record, then pad its names until the history replace
		// rebuilds is the size wanted.
		editState(func(st *State) {
			st.Serving = 99
			target := maxState - growthReserve + extra
			size, err := recordSize(*st)
			must(t, err)
			base, err := s.replaceable(*st)
			must(t, err)
			rebuilt, err := recordSize(base)
			must(t, err)
			padNames(t, st, size+target-rebuilt)
		})(t, dir)
		st, err := s.decode()
		must(t, err)
		base, err := s.replaceable(st)
		if extra == 0 {
			if err != nil {
				t.Fatalf("history at the boundary: %v", err)
			}
			if size, _ := recordSize(base); size != maxState-growthReserve {
				t.Fatalf("rebuilt history is %d bytes, want %d", size, maxState-growthReserve)
			}
			if err = s.ReplaceCA(); err != nil {
				t.Errorf("replace at the boundary: %v", err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "replace would number the new authority after generation 9 in state.json") {
			t.Errorf("replace one byte above the boundary: %v", err)
		}
	}
}

// commit's boundary: a record of exactly maxState bytes is saved and loads
// again; one byte more is refused and nothing is written. Guards the size
// check in commit (reviewer's R01 and R10).
func TestCommitSavesUpToWhatLoadReads(t *testing.T) {
	bases := boundBases(t)
	for _, size := range []int{maxState, maxState + 1} {
		s := outcomeStore(t, copyStore(t, bases[PhaseSingle]))
		st, err := s.State()
		must(t, err)
		padNames(t, &st, size)
		before := storeFiles(t, s.Dir())
		err = s.commit(st)
		switch {
		case size == maxState && err != nil:
			t.Errorf("commit at %d bytes: %v", size, err)
		case size == maxState:
			if _, err = s.State(); err != nil {
				t.Errorf("load at %d bytes: %v", size, err)
			}
		case err == nil || !strings.Contains(err.Error(), "refusing to save"):
			t.Errorf("commit at %d bytes: %v", size, err)
		default:
			if after := storeFiles(t, s.Dir()); !equalFiles(before, after) {
				t.Error("a refused save wrote the store")
			}
		}
	}
}

// What a started rotation can add before the store is single again is well
// inside the reserve: measured from the largest record prepare accepts, with
// a serving id that gains a digit and a full-precision timestamp, through
// prepare, switch and retire. The margin is the reserve's stated one.
func TestTheReserveCoversWhatFinishingAdds(t *testing.T) {
	bases := boundBases(t)
	start := maxState - growthReserve
	s := boundStore(t, bases[PhaseSingle], start)
	largest := start
	for _, m := range []move{movePrepare, moveSwitch, moveRetire, moveRenew} {
		must(t, m.run(s))
		largest = max(largest, storedSize(t, s.Dir()))
	}
	if grew := largest - start; grew > growthReserve/2 {
		t.Errorf("a rotation grew the record by %d bytes, more than half the %d-byte reserve", grew, growthReserve)
	} else {
		t.Logf("a rotation grew the record by %d bytes; the reserve is %d", grew, growthReserve)
	}
}
