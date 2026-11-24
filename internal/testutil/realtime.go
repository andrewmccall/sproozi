package testutil

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// BubbleHangLimit is how long, in real time, a test waits for a step of its
// synctest bubble before reporting that it never came: far above what any
// bubble step takes on a loaded machine. A step late by less is not caught.
const BubbleHangLimit = 30 * time.Second

// BubbleEndGrace is how long after reporting a watch waits for the bubble to
// end before it panics.
const BubbleEndGrace = 5 * time.Second

// RealTimeWatch reports, on real time, a synctest bubble that stops making
// progress. Nothing outside a bubble can wake a goroutine in it that waits on
// a lock or spins, so a hung bubble never lets synctest.Test return and the
// test would otherwise run to the binary's timeout with no message. Once
// armed, the watch waits its limit; then it prints the test's name and what
// it was waiting for, closes Expired, and panics if the bubble has not ended
// within its grace. Its channels belong to no bubble, so the bubble's clock
// does not drive it.
type RealTimeWatch struct {
	name         string
	limit, grace time.Duration
	waiting      atomic.Pointer[string]
	arm          sync.Once
	armed        chan struct{}
	expired      chan struct{}
	ended        chan struct{}
}

// WatchRealTime starts an unarmed watch for t with limit and grace. Call it,
// and End, outside the bubble; usually limit is BubbleHangLimit and grace
// BubbleEndGrace.
func WatchRealTime(t testing.TB, limit, grace time.Duration) *RealTimeWatch {
	w := &RealTimeWatch{name: t.Name(), limit: limit, grace: grace,
		armed: make(chan struct{}), expired: make(chan struct{}), ended: make(chan struct{})}
	go w.run()
	return w
}

// WatchBubble is WatchRealTime with BubbleHangLimit and BubbleEndGrace, armed
// at once with failure, the observation reported if the bubble hangs.
func WatchBubble(t testing.TB, failure string) *RealTimeWatch {
	w := WatchRealTime(t, BubbleHangLimit, BubbleEndGrace)
	w.WaitFor(failure)
	return w
}

// WaitFor arms the watch, once, and names what failed if the limit passes
// now, stated as an observation.
func (w *RealTimeWatch) WaitFor(failure string) {
	w.waiting.Store(&failure)
	w.arm.Do(func() { close(w.armed) })
}

// Expired is closed once the limit has passed.
func (w *RealTimeWatch) Expired() <-chan struct{} { return w.expired }

// End stops the watch once synctest.Test has returned.
func (w *RealTimeWatch) End() { close(w.ended) }

func (w *RealTimeWatch) run() {
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
