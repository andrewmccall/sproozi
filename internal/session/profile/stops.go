package profile

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
)

// Stops are the stops an executor carries on after their requests end (ADR
// 0036, amendment of 2026-11-16). A stop whose caller goes away mid-handoff
// would otherwise abandon a native quiesce whose reply nobody then reads, and
// that reply is the only proof that a harness such as Claude Code completed
// its turn. Each runs for at most StopBound. Cancellation still interrupts
// them: a placement abort of the same visit does so here, and the
// coordinator's own graceful interruption does so for an owner stop.
//
// Placement stops of one identical launch and visit share one flight: a stop
// asked while one runs waits for that one's answer instead of queuing another
// behind the visit's lock. Drain waits for every stop at shutdown. The zero
// value is ready.
type Stops struct {
	// Tail bounds Drain's wait after it interrupted the stops still running;
	// zero is DrainTail. Set it before the first stop.
	Tail time.Duration

	mu      sync.Mutex
	flights map[stopKey][]*stopFlight
	aborts  map[stopKey]int
	others  map[*stopFlight]struct{}
	// active counts the stops running; changed is closed, and replaced, each
	// time one ends.
	active  int
	changed chan struct{}
	closed  bool
	// joined, when set, is told each time a stop joins a running one.
	joined func()
}

// stopKey names one visit: its allocation and incarnation.
type stopKey struct {
	allocation  string
	incarnation session.Incarnation
}

type stopFlight struct {
	launch session.Launch
	cancel context.CancelFunc
	done   chan struct{}
	output response
	err    error
}

// errStopsClosed refuses a stop that arrives while the executor shuts down.
var errStopsClosed = errors.New("profile: executor is shutting down")

// errStopPanicked is what the callers that joined a stop get when it
// panicked: never an empty success.
var errStopPanicked = errors.New("profile: stop failed")

// ErrStopsLeftRunning reports stops that still ran after Drain interrupted
// them and waited DrainTail. They hold the placement's or the coordinator's
// locks, so closing either would wait for them.
var ErrStopsLeftRunning = errors.New("profile: stops still running after their interruption")

// begin detaches a stop from ctx, bounded by bound, and counts it for Drain;
// the caller holds s.mu and has checked s.closed. The context starts
// cancelled when cancelled is set.
func (s *Stops) begin(ctx context.Context, bound time.Duration, cancelled bool) (context.Context, *stopFlight) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bound)
	if cancelled {
		cancel()
	}
	s.active++
	return ctx, &stopFlight{cancel: cancel, done: make(chan struct{})}
}

// end uncounts a stop; the caller holds s.mu.
func (s *Stops) end() {
	s.active--
	if s.changed != nil {
		close(s.changed)
		s.changed = nil
	}
}

// Detach returns a context for an owner stop, or a recovery that continues
// one, that must outlive its request: it keeps ctx's values but not its
// cancellation, and ends after StopBound or when Drain gives up. Call the
// returned function when the stop ends. Once Drain began, the context is
// already cancelled, so the stop is refused.
func (s *Stops) Detach(ctx context.Context) (context.Context, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		refused, cancel := context.WithCancel(context.WithoutCancel(ctx))
		cancel()
		return refused, func() {}
	}
	ctx, f := s.begin(ctx, StopBound, false)
	if s.others == nil {
		s.others = map[*stopFlight]struct{}{}
	}
	s.others[f] = struct{}{}
	return ctx, func() {
		f.cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.others, f)
		s.end()
	}
}

// run performs a placement stop of input with stop, detached from ctx and
// bounded by bound, unless a stop of the same launch and visit already runs,
// whose answer it then returns. A caller that goes away stops waiting, not
// the stop.
func (s *Stops) run(ctx context.Context, input request, bound time.Duration, stop func(context.Context) (response, error)) (response, error) {
	key := stopKey{allocation: input.Launch.AllocationID, incarnation: *input.Incarnation}
	s.mu.Lock()
	for _, f := range s.flights[key] {
		if sameLaunch(f.launch, input.Launch) {
			joined := s.joined
			s.mu.Unlock()
			if joined != nil {
				joined()
			}
			select {
			case <-f.done:
				return f.output, f.err
			case <-ctx.Done():
				return response{}, ctx.Err()
			}
		}
	}
	if s.closed {
		s.mu.Unlock()
		return response{}, errStopsClosed
	}
	// An abort of this visit is running: the stop must not hold the visit
	// for a turn while the abort waits.
	flightCtx, f := s.begin(ctx, bound, s.aborts[key] > 0)
	f.launch = input.Launch
	if s.flights == nil {
		s.flights = map[stopKey][]*stopFlight{}
	}
	s.flights[key] = append(s.flights[key], f)
	s.mu.Unlock()
	finished := false
	defer func() {
		// A stop that panicked tells those who joined it that it failed,
		// then panics on, as the server expects of a handler.
		panicked := recover()
		if !finished {
			f.output, f.err = response{}, errStopPanicked
		}
		f.cancel()
		s.mu.Lock()
		remaining := s.flights[key][:0]
		for _, other := range s.flights[key] {
			if other != f {
				remaining = append(remaining, other)
			}
		}
		if len(remaining) == 0 {
			delete(s.flights, key)
		} else {
			s.flights[key] = remaining
		}
		s.end()
		s.mu.Unlock()
		close(f.done)
		if panicked != nil {
			panic(panicked)
		}
	}()
	f.output, f.err = stop(flightCtx)
	finished = true
	return f.output, f.err
}

// abort interrupts every stop of the visit input names and keeps any that
// begins from waiting for a turn, until the returned function runs. A stop
// of another incarnation, which the abort will refuse, is left alone.
func (s *Stops) abort(input request) func() {
	key := stopKey{allocation: input.Launch.AllocationID, incarnation: *input.Incarnation}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.aborts == nil {
		s.aborts = map[stopKey]int{}
	}
	s.aborts[key]++
	for _, f := range s.flights[key] {
		f.cancel()
	}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.aborts[key]--; s.aborts[key] == 0 {
			delete(s.aborts, key)
		}
	}
}

// idle waits until no stop runs or ctx ends, and reports how many still run.
func (s *Stops) idle(ctx context.Context) int {
	for {
		s.mu.Lock()
		if s.active == 0 {
			s.mu.Unlock()
			return 0
		}
		if s.changed == nil {
			s.changed = make(chan struct{})
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.active
		}
	}
}

// Drain refuses new stops and waits for the running ones to end. When ctx
// ends first it interrupts them and waits up to DrainTail for them to
// return; it then reports that they were cut short, or ErrStopsLeftRunning
// for those that still run, typically waiting for a lock that does not heed
// cancellation. Call it after the server stopped accepting requests, and
// before closing the placement and coordinator, whose locks they hold.
func (s *Stops) Drain(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	tail := s.Tail
	s.mu.Unlock()
	if s.idle(ctx) == 0 {
		return nil
	}
	s.mu.Lock()
	for _, flights := range s.flights {
		for _, f := range flights {
			f.cancel()
		}
	}
	for f := range s.others {
		f.cancel()
	}
	s.mu.Unlock()
	if tail == 0 {
		tail = DrainTail
	}
	waiting, cancel := context.WithTimeout(context.Background(), tail)
	defer cancel()
	if left := s.idle(waiting); left > 0 {
		return fmt.Errorf("%w: %d", ErrStopsLeftRunning, left)
	}
	return errors.New("profile: stops still running at shutdown were interrupted")
}
