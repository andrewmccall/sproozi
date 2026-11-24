package pki

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// errStopped is the crash a test injects at one write.
var errStopped = errors.New("test: process stopped")

// stopAt makes the store fail its nth write (counting from zero) and every
// write after it, as if the process had stopped there. It returns a counter
// of the writes attempted.
func stopAt(s *Store, n int) *int {
	count := 0
	s.beforeWrite = func(string, string) error {
		count++
		if count > n {
			return errStopped
		}
		return nil
	}
	return &count
}

// interruptedOperation is one operation from one state, as an operator,
// the controller or the local service runs it.
type interruptedOperation struct {
	from string
	name string
	run  func(s *Store) error
	// done reports whether the committed record already shows the
	// operation's effect, so a restart does not run it twice.
	done func(before, after State) bool
}

func interruptedOperations() []interruptedOperation {
	renew := func(s *Store) error { return s.Renew() }
	prepare := func(s *Store) error { return s.PrepareRotation() }
	switchOp := func(s *Store) error { return s.SwitchRotation(true) }
	rollback := func(s *Store) error { return s.RollbackRotation() }
	retire := func(s *Store) error { return s.RetireRotation(true) }
	replace := func(s *Store) error { return s.ReplaceCA() }
	stage := func(want Stage) func(_, after State) bool {
		return func(_, after State) bool { return after.Rotation != nil && after.Rotation.Stage == want }
	}
	renewed := func(before, after State) bool { return after.Leaf != before.Leaf }
	retired := func(_, after State) bool { return after.Rotation == nil }
	replaced := func(before, after State) bool { return len(after.Generations) > len(before.Generations) }
	return []interruptedOperation{
		{stSingle, "renew", renew, renewed},
		{stSingle, "prepare", prepare, stage(StagePrepared)},
		{stSingle, evNameReplace, replace, replaced},
		{stPrepared, evNameSwitch, switchOp, stage(StageSwitched)},
		{stPrepared, evNameReplace, replace, replaced},
		{stSwitched, "rollback", rollback, stage(StagePrepared)},
		{stSwitched, "retire", retire, retired},
		{stSwitched, evNameReplace, replace, replaced},
		{stReplacedKeyLeftPrepared, evNameSwitch, switchOp, stage(StageSwitched)},
	}
}

// restart reopens the store without the injected crash, runs what the
// local service runs at start and the controller on every reconcile
// (Ensure), runs the operation again if the committed record does not show
// it, as the operator or the service would, and runs Ensure again.
func restart(t *testing.T, root string, before State, op interruptedOperation) {
	t.Helper()
	s := outcomeStore(t, root)
	_, _ = s.Ensure(testNames)
	after, err := s.State()
	if err != nil {
		t.Fatalf("load after restart: %v", err)
	}
	if !op.done(before, after) {
		if err = op.run(s); err != nil {
			t.Fatalf("%s after restart: %v", op.name, err)
		}
	}
	if _, err = s.Ensure(testNames); err != nil {
		t.Fatalf("ensure after restart: %v", err)
	}
}

// settled describes a store's end state. The leaf's name is not compared: a
// retried renewal issues a different leaf. The number of leaf files is.
func settled(root string) string {
	return describeStore(root, leafOf(root))
}

// An interruption after every single write of every rotation operation,
// followed by a restart, ends in the same state as an uninterrupted run
// followed by the same restart.
func TestInterruptedOperationsConvergeAfterRestart(t *testing.T) {
	bases := map[string]string{}
	for _, state := range outcomeStates() {
		root := t.TempDir()
		state.build(t, outcomeStore(t, root))
		bases[state.name] = filepath.Join(root, "pki")
	}
	for _, op := range interruptedOperations() {
		ref := copyStore(t, bases[op.from])
		s := outcomeStore(t, ref)
		before, err := s.State()
		if err != nil {
			t.Fatal(err)
		}
		writes := stopAt(s, 1<<30)
		if err = op.run(s); err != nil {
			t.Fatalf("%s from %s: %v", op.name, op.from, err)
		}
		restart(t, ref, before, op)
		want := settled(ref)
		for n := range *writes {
			t.Run(fmt.Sprintf("%s from %s stopped at write %d", op.name, op.from, n), func(t *testing.T) {
				root := copyStore(t, bases[op.from])
				s := outcomeStore(t, root)
				attempted := stopAt(s, n)
				// Some removals ignore their error, so the operation may
				// report success; the write counter shows the stop was hit.
				_ = op.run(s)
				if *attempted <= n {
					t.Fatalf("write %d was not reached", n)
				}
				restart(t, root, before, op)
				if got := settled(root); got != want {
					t.Errorf("did not converge:\n got %s\nwant %s", got, want)
				}
			})
		}
	}
}
