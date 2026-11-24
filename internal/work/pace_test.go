package work

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/testutil"
	"github.com/google/uuid"
)

// A placement that reports the task's turn still pending at once cannot
// make the driver spin: one session's completion-wait rounds are at least
// completionPace apart, whether the wait began from stopping or from a
// recovered unknown stop.
//
// The driver runs in a synctest bubble, where a round takes no time, so
// the rounds fall at exactly 0, 1 and 2 paces of the bubble's clock; on
// real time a slow round could push the third past the period. A driver
// that spins never lets the bubble's clock reach the end of the period,
// and the watch on real time reports it (see testutil.RealTimeWatch).
func TestCompletionWaitIsPaced(t *testing.T) {
	t.Parallel()
	template.once.Do(func() { template.err = buildTemplate() })
	for _, state := range []session.Phase{session.PhaseStopping, session.PhaseUnknownStop} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			watch := testutil.WatchRealTime(t, testutil.BubbleHangLimit, testutil.BubbleEndGrace)
			defer watch.End()
			synctest.Test(t, func(t *testing.T) {
				e := newDriverEnv(t, false, clockNone)
				namedJournalState(t, string(state)).build(t, e)
				e.fixture(func(p *fixturePlacement) { p.turnPending = 1 << 30 })
				_ = e.p.counts()
				period := 2*completionPace + completionPace/2
				ctx, cancel := context.WithTimeout(t.Context(), period)
				defer cancel()
				watch.WaitFor("the driver did not stop at the end of the period on the bubble's clock")
				e.o.drive(ctx, e.id, make(chan struct{}))
				e.p.mu.Lock()
				stops := e.p.calls["stop"]
				e.p.mu.Unlock()
				if stops != 3 {
					t.Fatalf("%d placement stops in %s, want 3 at a pace of %s", stops, period, completionPace)
				}
			})
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
//
// The driver runs in a synctest bubble, whose clock moves only while every
// goroutine in it waits on the bubble. The test waits for the cancel on a
// channel from outside the bubble, so the clock stands still and the pause
// cannot end until the cancel returns. A cancel that waits for anything the
// paused driver or the bubble's clock would release therefore never
// returns, and a watch on real time reports it (see testutil.RealTimeWatch).
//
// The fixture template is built before the bubble starts, so its git
// subprocesses run outside it. Capturing the first visit's input, which
// newDriverEnv's first caller does, can still run git inside the bubble:
// the clock does not move while a subprocess runs, which is all this test
// needs of it.
func TestCancellationIsNotHeldByTheCompletionPause(t *testing.T) {
	t.Parallel()
	template.once.Do(func() { template.err = buildTemplate() })
	watch := testutil.WatchRealTime(t, testutil.BubbleHangLimit, testutil.BubbleEndGrace)
	defer watch.End()
	synctest.Test(t, func(t *testing.T) {
		e := newDriverEnv(t, false, clockNone)
		namedJournalState(t, string(session.PhaseStopping)).build(t, e)
		e.fixture(func(p *fixturePlacement) { p.turnPending = 1 << 30 })
		_ = e.p.counts()
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
		// A round takes no time on the bubble's clock, so a quarter of the
		// pace in, the driver has taken one round and waits out its pause.
		watch.WaitFor("the driver did not take its first completion-wait round and pause")
		time.Sleep(completionPace / 4)
		e.p.mu.Lock()
		rounds := e.p.calls["stop"]
		e.p.mu.Unlock()
		if rounds != 1 {
			t.Fatalf("%d completion-wait rounds before the cancel, want 1", rounds)
		}
		type cancelled struct {
			v   View
			err error
		}
		result := make(chan cancelled, 1)
		paused := "the cancel did not return while the driver was paused"
		watch.WaitFor(paused)
		go func() {
			v, err := e.o.Cancel(t.Context(), e.id, uuid.NewString())
			result <- cancelled{v, err}
		}()
		var r cancelled
		select {
		case r = <-result:
		case <-watch.Expired():
			// Waiting on the bubble alone lets its clock move again, so a
			// cancel held by the pause or the clock returns and the test
			// ends; one held by a lock does not, and the watch panics.
			t.Errorf("within %s of real time, %s", testutil.BubbleHangLimit, paused)
			watch.WaitFor("the cancel did not return once the bubble's clock could move again")
			r = <-result
		}
		watch.WaitFor("the driver and the owner did not stop after the cancel")
		if r.err != nil || r.v.Phase != phaseCancelled {
			t.Fatalf("cancel during the pause: %s %v", r.v.Phase, r.err)
		}
	})
}
