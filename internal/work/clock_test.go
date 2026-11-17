package work

import (
	"testing"
	"time"
)

// The driver decides a deadline on the coordinator's clock, the one the
// coordinator refuses a start by, so the two cannot disagree. Here the
// deadline has passed for the coordinator but not for the wall clock: an
// unreceived task is ended as expired, and a starting one is ended rather
// than recovered.
func TestDriverDecidesTheDeadlineOnTheCoordinatorsClock(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"none", "preparing"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newDriverEnv(t, false, clockEarly)
			namedJournalState(t, name).build(t, e)
			e.offset.Store(int64(2 * time.Hour))
			_ = e.p.counts()
			if n := e.o.step(t.Context(), e.id); n != nextAgain {
				t.Fatalf("step: %s, want again", nextNames[n])
			}
			r, err := e.o.read(e.id)
			if err != nil {
				t.Fatal(err)
			}
			if r.Problem != problemExpired {
				t.Fatalf("problem %q, want %q", r.Problem, problemExpired)
			}
			if calls := e.p.counts(); calls != "calls=none" && calls != "calls=inspect=1" {
				t.Fatalf("the driver tried to start the task: %s", calls)
			}
			// Ended by the driver's decision, not by the coordinator
			// abandoning a start the driver asked it to recover.
			cancelled := r.CancelOperation
			if rec, err := e.o.execution(r); err != nil {
				t.Fatal(err)
			} else if rec != nil {
				cancelled = rec.CancelOperation
			}
			if cancelled != derivedOperation(e.id, "deadline-expired") {
				t.Fatalf("the task was not cancelled as expired: %s", e.opName(cancelled))
			}
		})
	}
}
