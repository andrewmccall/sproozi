package profile

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

func stopInput() request {
	l := launch(strings.Repeat("b", 64))
	i := session.Incarnation{ExecutionID: l.AllocationID, ResourceUID: uuid.NewString()}
	return request{Launch: l, Incarnation: &i}
}

// A stop that panics tells those who joined it that it failed, never that
// it succeeded with nothing, and panics on.
func TestJoinerOfAPanickingStopGetsAnError(t *testing.T) {
	t.Parallel()
	input := stopInput()
	var s Stops
	joined := make(chan struct{}, 1)
	s.joined = func() { joined <- struct{}{} }
	started, release := make(chan struct{}), make(chan struct{})
	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		_, _ = s.run(t.Context(), input, time.Minute, func(context.Context) (response, error) {
			close(started)
			<-release
			panic("stop failed")
		})
	}()
	awaitSignal(t, started, "the stop starting")
	answer := make(chan error, 1)
	go func() {
		out, err := s.run(t.Context(), input, time.Minute, func(context.Context) (response, error) {
			return response{}, errors.New("the joiner ran a stop of its own")
		})
		if err == nil && out.Stopped == nil {
			err = errors.New("empty success")
		}
		answer <- err
	}()
	awaitSignal(t, joined, "the joiner joining")
	close(release)
	if p := <-panicked; p == nil {
		t.Fatal("the stop's panic was swallowed")
	}
	if err := <-answer; !errors.Is(err, errStopPanicked) {
		t.Fatalf("the joiner of a panicking stop got %v", err)
	}
	if err := s.Drain(t.Context()); err != nil {
		t.Fatalf("a panicking stop stayed counted: %v", err)
	}
}

// An abort whose placement panics still lets later stops of the visit wait
// for their turn.
func TestAbortThatPanicsStillReleasesTheVisit(t *testing.T) {
	t.Parallel()
	_, b, h, l, i, _ := heldFixture(t, 0)
	b.abortPanics = true
	input := request{Launch: l, Incarnation: &i}
	func() {
		defer func() { _ = recover() }()
		_, _ = h.abort(t.Context(), input)
	}()
	observed := func(ctx context.Context) (response, error) { return response{}, ctx.Err() }
	if _, err := h.stops.run(t.Context(), input, time.Minute, observed); err != nil {
		t.Fatalf("a stop after a panicked abort started with %v", err)
	}
}

// An owner stop that arrives after the drain began is refused and is never
// counted, so the drain's wait cannot grow under it.
func TestOwnerStopAfterDrainIsRefused(t *testing.T) {
	t.Parallel()
	var s Stops
	if err := s.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, done := s.Detach(t.Context())
	defer done()
	if ctx.Err() == nil {
		t.Fatal("an owner stop began after the drain")
	}
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	if active != 0 {
		t.Fatalf("a refused owner stop was counted: %d", active)
	}
}

// A stop that does not let go once interrupted, such as one waiting for a
// lock that ignores cancellation, ends the drain after its tail with
// ErrStopsLeftRunning rather than holding shutdown.
func TestDrainGivesUpOnAStopThatIgnoresItsInterruption(t *testing.T) {
	t.Parallel()
	s := Stops{Tail: 100 * time.Millisecond}
	_, done := s.Detach(t.Context())
	release := make(chan struct{})
	go func() {
		<-release
		done()
	}()
	defer close(release)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := s.Drain(ctx)
	if !errors.Is(err, ErrStopsLeftRunning) || !strings.Contains(err.Error(), ": 1") {
		t.Fatalf("drain of a stop that ignores its interruption: %v", err)
	}
	if waited := time.Since(started); waited > 5*time.Second {
		t.Fatalf("drain waited %s", waited)
	}
}
