package nativepty

import "time"

// deadlineCheck is how often AtDeadline reads the wall clock.
var deadlineCheck = time.Second

// wallClock reads the time without its monotonic reading, so a comparison
// uses the wall clock.
var wallClock = func() time.Time { return time.Now().Round(0) }

// AtDeadline returns a channel that closes once the wall clock reaches
// deadline, or never if stop closes first. It reads the wall clock every
// second rather than waiting for the remaining duration, because a timer
// counts monotonic time, which stands still while the host sleeps: in a
// Docker Desktop VM, a Mac that slept for half an hour would otherwise end a
// session half an hour late.
func AtDeadline(deadline time.Time, stop <-chan struct{}) <-chan struct{} {
	reached := make(chan struct{})
	go func() {
		ticker := time.NewTicker(deadlineCheck)
		defer ticker.Stop()
		for wallClock().Before(deadline) {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
		close(reached)
	}()
	return reached
}
