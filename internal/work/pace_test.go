package work

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
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
//
// The driver runs in a synctest bubble, where a round takes no time, so
// the rounds fall at exactly 0, 1 and 2 paces of the bubble's clock; on
// real time a slow round could push the third past the period. A driver
// that spins never lets the bubble's clock reach the end of the period,
// and the watch on real time reports it (see watchRealTime).
func TestCompletionWaitIsPaced(t *testing.T) {
	t.Parallel()
	template.once.Do(func() { template.err = buildTemplate() })
	for _, state := range []session.Phase{session.PhaseStopping, session.PhaseUnknownStop} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			watch := watchRealTime(t, cancelHangLimit, bubbleEndLimit)
			defer watch.end()
			synctest.Test(t, func(t *testing.T) {
				e := newDriverEnv(t, false, clockNone)
				namedJournalState(t, string(state)).build(t, e)
				e.fixture(func(p *fixturePlacement) { p.turnPending = 1 << 30 })
				_ = e.p.counts()
				period := 2*completionPace + completionPace/2
				ctx, cancel := context.WithTimeout(t.Context(), period)
				defer cancel()
				watch.waitFor("the driver did not stop at the end of the period on the bubble's clock")
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
// returns, and a watch on real time reports it (see watchRealTime).
//
// The fixture template is built before the bubble starts, so its git
// subprocesses run outside it. Capturing the first visit's input, which
// newDriverEnv's first caller does, can still run git inside the bubble:
// the clock does not move while a subprocess runs, which is all this test
// needs of it.
func TestCancellationIsNotHeldByTheCompletionPause(t *testing.T) {
	t.Parallel()
	template.once.Do(func() { template.err = buildTemplate() })
	watch := watchRealTime(t, cancelHangLimit, bubbleEndLimit)
	defer watch.end()
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
		watch.waitFor("the driver did not take its first completion-wait round and pause")
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
		watch.waitFor(paused)
		go func() {
			v, err := e.o.Cancel(t.Context(), e.id, uuid.NewString())
			result <- cancelled{v, err}
		}()
		var r cancelled
		select {
		case r = <-result:
		case <-watch.expired:
			// Waiting on the bubble alone lets its clock move again, so a
			// cancel held by the pause or the clock returns and the test
			// ends; one held by a lock does not, and the watch panics.
			t.Errorf("within %s of real time, %s", cancelHangLimit, paused)
			watch.waitFor("the cancel did not return once the bubble's clock could move again")
			r = <-result
		}
		watch.waitFor("the driver and the owner did not stop after the cancel")
		if r.err != nil || r.v.Phase != phaseCancelled {
			t.Fatalf("cancel during the pause: %s %v", r.v.Phase, r.err)
		}
	})
}

// cancelHangLimit is how long, in real time, the cancel test waits for its
// bubble's steps: a cancel takes well under two seconds on a loaded machine.
const cancelHangLimit = 30 * time.Second

// bubbleEndLimit is how long after reporting the watch waits for the
// bubble to end before it panics.
const bubbleEndLimit = 5 * time.Second

// realTimeWatch reports, on real time, a synctest bubble that stops making
// progress. Nothing outside a bubble can wake a goroutine in it that waits
// on a lock or on the bubble's clock, so a hung bubble never lets
// synctest.Test return. Once armed, the watch waits limit; then it prints
// the test's name and what it was waiting for, closes expired, and panics
// if the bubble has not ended within grace more. Its channels belong to no
// bubble, so the bubble's clock does not drive it.
type realTimeWatch struct {
	name         string
	limit, grace time.Duration
	waiting      atomic.Pointer[string]
	arm          sync.Once
	armed        chan struct{}
	expired      chan struct{}
	ended        chan struct{}
}

// watchRealTime starts an unarmed watch for t; call it, and end, outside
// the bubble.
func watchRealTime(t *testing.T, limit, grace time.Duration) *realTimeWatch {
	w := &realTimeWatch{name: t.Name(), limit: limit, grace: grace,
		armed: make(chan struct{}), expired: make(chan struct{}), ended: make(chan struct{})}
	go w.run()
	return w
}

// waitFor arms the watch, once, and names what failed if the limit passes
// now, stated as an observation.
func (w *realTimeWatch) waitFor(failure string) {
	w.waiting.Store(&failure)
	w.arm.Do(func() { close(w.armed) })
}

// end stops the watch once synctest.Test has returned.
func (w *realTimeWatch) end() { close(w.ended) }

func (w *realTimeWatch) run() {
	select {
	case <-w.armed:
	case <-w.ended:
		return
	}
	limit := time.NewTimer(w.limit)
	defer limit.Stop()
	select {
	case <-limit.C:
	case <-w.ended:
		return
	}
	failure := fmt.Sprintf("%s: within %s of real time, %s", w.name, w.limit, *w.waiting.Load())
	_, _ = fmt.Fprintln(os.Stderr, "--- FAIL: "+failure)
	close(w.expired)
	grace := time.NewTimer(w.grace)
	defer grace.Stop()
	select {
	case <-grace.C:
		panic(fmt.Sprintf("%s; its synctest bubble did not end within %s more (%s)",
			failure, w.grace, *w.waiting.Load()))
	case <-w.ended:
	}
}
