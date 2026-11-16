package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/session/profile"
)

// Shutdown waits for a stop the executor carries on after its request ended,
// so the stop reaches its outcome before the placement closes.
func TestShutdownWaitsForAStopInFlight(t *testing.T) {
	var stops profile.Stops
	ctx, done := stops.Detach(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer done()
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
			t.Error("shutdown interrupted a stop within its drain bound")
		}
	}()
	var log bytes.Buffer
	if err := shutdown(&http.Server{}, &stops, 5*time.Second, &log); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("shutdown returned before the stop ended")
	}
	if log.Len() != 0 {
		t.Fatalf("shutdown logged %q", log.String())
	}
}

// A stop that does not let go once interrupted, such as one queued on a lock
// that ignores cancellation, would keep the deferred closes waiting until the
// kubelet's kill; shutdown exits instead and says what it left.
func TestShutdownExitsWhenAStopIgnoresItsInterruption(t *testing.T) {
	previous := exit
	code := -1
	exit = func(c int) { code = c }
	t.Cleanup(func() { exit = previous })
	stops := profile.Stops{Tail: 100 * time.Millisecond}
	_, done := stops.Detach(context.Background())
	release := make(chan struct{})
	go func() {
		<-release
		done()
	}()
	defer close(release)
	var log bytes.Buffer
	if err := shutdown(&http.Server{}, &stops, 50*time.Millisecond, &log); err != nil {
		t.Fatal(err)
	}
	if code != 1 || !strings.Contains(log.String(), "Exited without closing the placement") {
		t.Fatalf("shutdown exited %d and logged %q", code, log.String())
	}
}

// Past its drain bound, shutdown interrupts the stop, waits for it to let go
// and says so, so the deferred closes do not wait on its locks.
func TestShutdownInterruptsAStopPastItsDrainBound(t *testing.T) {
	var stops profile.Stops
	ctx, done := stops.Detach(context.Background())
	go func() {
		defer done()
		<-ctx.Done()
	}()
	var log bytes.Buffer
	started := time.Now()
	if err := shutdown(&http.Server{}, &stops, 100*time.Millisecond, &log); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil || time.Since(started) > 5*time.Second {
		t.Fatalf("stop left running %s after shutdown", time.Since(started))
	}
	if !strings.Contains(log.String(), "Interrupted stops still running after 100ms at shutdown") {
		t.Fatalf("shutdown logged %q", log.String())
	}
}
