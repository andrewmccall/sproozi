package profile

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

// heldStop starts a stop that the backend holds until release, then the
// client goes away; it returns once the executor noticed.
func heldStop(t *testing.T, c *Client, b *heldBackend, l session.Launch, i session.Incarnation, ended <-chan struct{}) {
	t.Helper()
	b.entered, b.release = make(chan struct{}), make(chan struct{})
	entered := b.entered
	ctx, leave := context.WithCancel(t.Context())
	first := make(chan error, 1)
	go func() {
		_, err := c.Stop(ctx, l, i)
		first <- err
	}()
	awaitSignal(t, entered, "the stop starting")
	leave()
	<-first
	awaitSignal(t, ended, "the executor noticing that home left")
}

// At shutdown the executor waits for a stop it carries on to reach its
// outcome before it closes the placement.
func TestDrainWaitsForAStopWhoseRequestEnded(t *testing.T) {
	t.Parallel()
	c, b, h, l, i, ended := heldFixture(t, 0)
	heldStop(t, c, b, l, i, ended)
	drained := make(chan error, 1)
	go func() { drained <- h.stops.Drain(t.Context()) }()
	select {
	case err := <-drained:
		t.Fatalf("drain returned while a stop ran: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(b.release)
	select {
	case err := <-drained:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not return once the stop ended")
	}
	if ends := b.stopEnds(); len(ends) != 1 || ends[0] != nil {
		t.Fatalf("the drained stop ended with %v", ends)
	}
}

// Past its bound, drain interrupts the stops still running, waits for them
// to let go, and refuses new ones.
func TestDrainInterruptsStopsPastItsBoundAndRefusesNewOnes(t *testing.T) {
	t.Parallel()
	c, b, h, l, i, ended := heldFixture(t, 0)
	heldStop(t, c, b, l, i, ended)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := h.stops.Drain(ctx); err == nil {
		t.Fatal("drain past its bound reported every stop finished")
	}
	if ends := b.stopEnds(); len(ends) != 1 || !errors.Is(ends[0], context.Canceled) {
		t.Fatalf("the interrupted stop ended with %v", ends)
	}
	_, err := c.Stop(t.Context(), l, i)
	remoteCode(t, err, CodeUnavailable, http.StatusServiceUnavailable)
	if len(b.stopEnds()) != 1 {
		t.Fatal("a stop began after drain")
	}
}

// Stops of one visit asked while one runs share its answer, so retries from a
// home that keeps timing out do not queue stops behind the visit's lock.
func TestStopsOfOneVisitShareOneFlight(t *testing.T) {
	t.Parallel()
	c, b, h, l, i, ended := heldFixture(t, 0)
	joined := make(chan struct{}, 2)
	h.stops.mu.Lock()
	h.stops.joined = func() { joined <- struct{}{} }
	h.stops.mu.Unlock()
	heldStop(t, c, b, l, i, ended)
	retried := make(chan error, 2)
	for range 2 {
		go func() {
			s, err := c.Stop(t.Context(), l, i)
			if err == nil && s.Receipt == nil {
				err = errors.New("retry answered without a receipt")
			}
			retried <- err
		}()
	}
	awaitSignal(t, joined, "the first retry joining the stop")
	awaitSignal(t, joined, "the second retry joining the stop")
	close(b.release)
	for range 2 {
		select {
		case err := <-retried:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a retry did not get the stop's answer")
		}
	}
	if ends := b.stopEnds(); len(ends) != 1 {
		t.Fatalf("one visit's concurrent stops ran %d times", len(ends))
	}
}

// An abort that names another incarnation, which the placement refuses,
// does not cut the visit's stop short.
func TestStaleAbortLeavesTheStopRunning(t *testing.T) {
	t.Parallel()
	c, b, _, l, i, ended := heldFixture(t, 0)
	heldStop(t, c, b, l, i, ended)
	stale := i
	stale.ResourceUID = uuid.NewString()
	aborted := make(chan error, 1)
	go func() {
		_, err := c.Abort(t.Context(), l, stale)
		aborted <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if ends := b.stopEnds(); len(ends) != 0 {
		t.Fatalf("a stale abort interrupted the stop: %v", ends)
	}
	close(b.release)
	select {
	case err := <-aborted:
		remoteCode(t, err, CodeConflict, http.StatusConflict)
	case <-time.After(5 * time.Second):
		t.Fatal("the stale abort did not return")
	}
	if ends := b.stopEnds(); len(ends) != 1 || ends[0] != nil {
		t.Fatalf("the stop ended with %v", ends)
	}
}

// A stop that begins while an abort of the same visit runs starts already
// cancelled, so it cannot hold the visit for a turn while the abort waits;
// once the abort ends, stops wait as usual.
func TestStopBegunDuringAnAbortStartsCancelled(t *testing.T) {
	t.Parallel()
	l := launch(strings.Repeat("b", 64))
	i := session.Incarnation{ExecutionID: l.AllocationID, ResourceUID: uuid.NewString()}
	input := request{Launch: l, Incarnation: &i}
	var s Stops
	release := s.abort(input)
	observed := func(ctx context.Context) (response, error) { return response{}, ctx.Err() }
	if _, err := s.run(t.Context(), input, time.Minute, observed); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop during an abort ran with %v", err)
	}
	release()
	if _, err := s.run(t.Context(), input, time.Minute, observed); err != nil {
		t.Fatalf("stop after the abort ran with %v", err)
	}
	if err := s.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
}
