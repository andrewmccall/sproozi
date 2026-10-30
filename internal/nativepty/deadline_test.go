package nativepty

import (
	"sync/atomic"
	"testing"
	"time"
)

// A host that slept past the deadline ends the session at the next check,
// although the monotonic clock says the deadline is still far off.
func TestAtDeadlineFollowsTheWallClockAfterSleep(t *testing.T) {
	start := time.Now().Round(0)
	deadline := start.Add(time.Hour)
	var slept atomic.Bool
	check, clock := deadlineCheck, wallClock
	deadlineCheck = 10 * time.Millisecond
	wallClock = func() time.Time {
		if slept.Load() {
			return deadline.Add(time.Minute)
		}
		return start
	}
	t.Cleanup(func() { deadlineCheck, wallClock = check, clock })
	reached := AtDeadline(deadline, nil)
	select {
	case <-reached:
		t.Fatal("reached the deadline before it")
	case <-time.After(50 * time.Millisecond):
	}
	slept.Store(true)
	select {
	case <-reached:
	case <-time.After(time.Second):
		t.Fatal("the deadline passed on the wall clock but was not reached")
	}
}

func TestAtDeadlineStopsWithoutReaching(t *testing.T) {
	stop := make(chan struct{})
	reached := AtDeadline(time.Now().Add(time.Hour), stop)
	close(stop)
	select {
	case <-reached:
		t.Fatal("a stopped wait reached its deadline")
	case <-time.After(50 * time.Millisecond):
	}
}
