package work

import (
	"context"
	"testing"
	"testing/synctest"
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
//
// The driver runs in a synctest bubble, whose clock moves only while every
// goroutine in it waits on the bubble. The test waits for the cancel on a
// channel from outside the bubble, so the clock stands still and the pause
// cannot end until the cancel returns: a cancel held by the pause never
// returns, and is reported once cancelHangLimit of real time has passed.
func TestCancellationIsNotHeldByTheCompletionPause(t *testing.T) {
	t.Parallel()
	arm, expired := realTimeLimit(t, cancelHangLimit)
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
		close(arm)
		go func() {
			v, err := e.o.Cancel(t.Context(), e.id, uuid.NewString())
			result <- cancelled{v, err}
		}()
		select {
		case r := <-result:
			if r.err != nil || r.v.Phase != phaseCancelled {
				t.Fatalf("cancel during the pause: %s %v", r.v.Phase, r.err)
			}
		case <-expired:
			t.Fatalf("cancel during a pause of %s did not return in %s: it waits for the pause", completionPace, cancelHangLimit)
		}
	})
}

// cancelHangLimit is how long a test waits for a cancel that can only
// return if it does not wait for the driver: a cancel takes well under two
// seconds on a loaded machine.
const cancelHangLimit = 30 * time.Second

// realTimeLimit returns a channel to close, from inside a synctest bubble,
// when d of real time starts, and a channel closed when it has passed. Both
// belong to no bubble, so the bubble's clock neither drives the limit nor
// moves while a test waits on it.
func realTimeLimit(t *testing.T, d time.Duration) (arm chan<- struct{}, expired <-chan struct{}) {
	armed, passed := make(chan struct{}), make(chan struct{})
	done := t.Context().Done()
	go func() {
		select {
		case <-armed:
		case <-done:
			return
		}
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
			close(passed)
		case <-done:
		}
	}()
	return armed, passed
}
