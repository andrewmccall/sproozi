package proxytransport

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

// Model call duration. A model response streams for as long as the model
// generates, which can outlast the inspected connection that carries it. A
// model handler asks for its own deadline with ExtendModelCall; other
// requests keep MaxRequestDuration.
const (
	DefaultMaxModelCallDuration = 20 * time.Minute
	MaxModelCallDurationLimit   = time.Hour
)

// ErrModelCallDeadline is the cause of a model call context that reached its
// maximum duration.
var ErrModelCallDeadline = errors.New("model call reached its maximum duration")

type modelCallKey struct{}

// defaultModelCall selects and checks the maximum model call duration.
func (c *Config) defaultModelCall() error {
	if c.MaxModelCallDuration == 0 {
		c.MaxModelCallDuration = DefaultMaxModelCallDuration
	}
	if c.MaxModelCallDuration < time.Second || c.MaxModelCallDuration > MaxModelCallDurationLimit {
		return errors.New("invalid model call bound")
	}
	return nil
}

// modelCallGrant lets one inspected request outlive its connection's request
// deadline. base is the server's own request context: it ends when the client
// disconnects, the identity is revoked or the connection closes, but not at
// the request deadline.
type modelCallGrant struct {
	base     context.Context
	start    time.Time
	max      time.Duration
	session  *sessionLife
	mu       sync.Mutex
	extended bool
}

func (g *modelCallGrant) wasExtended() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.extended
}

// ExtendModelCall returns the context for one model call: it lasts from the
// start of the inspected request until min(the gateway's maximum model call
// duration, limit), even past the connection's request deadline. limit zero
// keeps the gateway maximum, so a policy can only lower it. Revocation, a
// client disconnect and the end of the connection still cancel the call, with
// their cause. While the call runs the connection stays open for it alone: it
// still stops keep-alive and refuses new requests at its request deadline.
//
// Outside an inspected connection, as in tests, the call lasts at most limit.
// The returned function must be called when the call ends.
func ExtendModelCall(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	grant, ok := ctx.Value(modelCallKey{}).(*modelCallGrant)
	if !ok {
		if limit > 0 {
			return context.WithTimeoutCause(ctx, limit, ErrModelCallDeadline)
		}
		return context.WithCancel(ctx)
	}
	duration := grant.max
	if limit > 0 && limit < duration {
		duration = limit
	}
	deadline := grant.start.Add(duration)
	// Keep the request's values but not its deadline; take every other
	// cancellation, with its cause, from the server's request context.
	detached, cancelDetached := context.WithCancelCause(context.WithoutCancel(ctx))
	stop := context.AfterFunc(grant.base, func() { cancelDetached(context.Cause(grant.base)) })
	callCtx, cancelCall := context.WithDeadlineCause(detached, deadline, ErrModelCallDeadline)
	grant.mu.Lock()
	grant.extended = true
	grant.mu.Unlock()
	release := grant.session.hold(deadline)
	return callCtx, func() {
		stop()
		cancelCall()
		cancelDetached(context.Canceled)
		release()
	}
}

// sessionLife ends an inspected connection at its lifetime, or when the last
// model call holding it past its lifetime ends.
type sessionLife struct {
	mu         sync.Mutex
	connection net.Conn
	end        context.CancelFunc
	grace      time.Duration
	expired    bool
	held       int
	writeUntil time.Time
}

// hold keeps the connection open and writable until until, plus the grace
// the server needs to finish a reply.
func (s *sessionLife) hold(until time.Time) func() {
	s.mu.Lock()
	s.held++
	if writeUntil := until.Add(s.grace); writeUntil.After(s.writeUntil) {
		s.writeUntil = writeUntil
		_ = s.connection.SetWriteDeadline(writeUntil)
	}
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.held--
			if s.expired && s.held == 0 {
				// Keep-alive is already off, so the server closes the
				// connection once it has finished the reply; end the session
				// after the grace in case it does not.
				time.AfterFunc(s.grace, s.end)
			}
		})
	}
}

func (s *sessionLife) expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expired = true
	if s.held == 0 {
		s.end()
	}
}
