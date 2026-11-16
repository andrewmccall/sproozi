package profile

import (
	"context"
	"sync"
	"time"
)

// DetachStop returns a context for a stop that must outlive its request: it
// keeps ctx's values but not its cancellation, and ends after StopBound. A
// stop whose caller goes away mid-handoff would otherwise abandon a native
// quiesce whose reply then nobody reads, and that reply is the only proof
// that a harness such as Claude Code completed its turn. Whoever cancels the
// execution must still be able to interrupt it, through the returned cancel
// or the coordinator's own graceful interruption (ADR 0036, amendment of
// 2026-11-16).
func DetachStop(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), StopBound)
}

// stops are the stops an executor carries on after their requests end, by
// allocation, so that an abort of the same visit interrupts them: cancellation
// never waits for a turn.
type stops struct {
	mu      sync.Mutex
	running map[string]map[*stopFlight]struct{}
}

type stopFlight struct{ cancel context.CancelFunc }

// begin detaches a stop of allocation from its request, bounded by bound,
// and returns its context and the function that ends it.
func (s *stops) begin(ctx context.Context, allocation string, bound time.Duration) (context.Context, func()) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bound)
	f := &stopFlight{cancel: cancel}
	s.mu.Lock()
	if s.running == nil {
		s.running = map[string]map[*stopFlight]struct{}{}
	}
	if s.running[allocation] == nil {
		s.running[allocation] = map[*stopFlight]struct{}{}
	}
	s.running[allocation][f] = struct{}{}
	s.mu.Unlock()
	return ctx, func() {
		s.mu.Lock()
		delete(s.running[allocation], f)
		if len(s.running[allocation]) == 0 {
			delete(s.running, allocation)
		}
		s.mu.Unlock()
		cancel()
	}
}

// interrupt ends every stop of allocation the executor is carrying on.
func (s *stops) interrupt(allocation string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for f := range s.running[allocation] {
		f.cancel()
	}
}
