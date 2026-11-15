package nativepty_test

import (
	"testing"
	"time"
)

// A process launched without a deadline runs and is stopped only when asked
// (ADR 0056); start's cleanup stops it.
func TestProcessWithoutDeadlineRunsUntilStopped(t *testing.T) {
	s := start(t, `stty -echo; printf 'READY\n'; exec sleep 30`, 256, time.Time{})
	a := attach(t, s, 0)
	until(t, a, "READY")
	select {
	case <-s.Done():
		t.Fatal("a process without a deadline ended on its own")
	case <-time.After(1500 * time.Millisecond):
	}
}
