package work

import (
	"context"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

// A placement that reports the task's turn still pending at once cannot
// make the driver spin: one session's completion-wait rounds are at least
// completionPace apart, whether the wait began from stopping or from a
// recovered unknown stop.
func TestCompletionWaitIsPaced(t *testing.T) {
	t.Parallel()
	for _, state := range []session.Phase{session.PhaseStopping, session.PhaseUnknownStop} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			e := newDriverEnv(t, false, clockNone)
			namedJournalState(t, string(state)).build(t, e)
			e.fixture(func(p *fixturePlacement) { p.turnPending = 1 << 30 })
			_ = e.p.counts()
			period := 2*completionPace + completionPace/2
			ctx, cancel := context.WithTimeout(t.Context(), period)
			defer cancel()
			e.o.drive(ctx, e.id, make(chan struct{}))
			e.p.mu.Lock()
			stops := e.p.calls["stop"]
			e.p.mu.Unlock()
			// Rounds at 0, 1 and 2 paces, and one more for scheduling.
			if stops < 2 || stops > 4 {
				t.Fatalf("%d placement stops in %s, want 3 at a pace of %s", stops, period, completionPace)
			}
		})
	}
}

// A round that already took longer than the pace waits no extra, and one
// that took part of it waits only the rest.
func TestCompletionPauseCountsTheRoundsOwnTime(t *testing.T) {
	for _, tc := range []struct{ took, want time.Duration }{
		{0, completionPace},
		{completionPace / 4, completionPace * 3 / 4},
		{completionPace, 0},
		{2 * time.Minute, 0},
	} {
		if got := completionPause(tc.took); got != tc.want {
			t.Errorf("after a round of %s: pause %s, want %s", tc.took, got, tc.want)
		}
	}
}

// The pause never holds up a cancellation the owner asks for: it goes
// through the coordinator, which interrupts the completion wait, not
// through the paused driver.
func TestCancellationIsNotHeldByTheCompletionPause(t *testing.T) {
	t.Parallel()
	e := newDriverEnv(t, false, clockNone)
	namedJournalState(t, string(session.PhaseStopping)).build(t, e)
	e.fixture(func(p *fixturePlacement) { p.turnPending = 1 << 30 })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.o.drive(ctx, e.id, make(chan struct{}))
	}()
	defer func() {
		cancel()
		<-done
	}()
	// Let the driver take a round and pause.
	time.Sleep(completionPace / 4)
	began := time.Now()
	v, err := e.o.Cancel(t.Context(), e.id, uuid.NewString())
	if err != nil || v.Phase != phaseCancelled {
		t.Fatalf("cancel during the pause: %s %v", v.Phase, err)
	}
	if took := time.Since(began); took >= completionPace/2 {
		t.Fatalf("cancel took %s during a pause of %s", took, completionPace)
	}
}
