package proxytransport

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/andrewmccall/sproozi/internal/gateway"
)

func acquire(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// LimitListener admits sockets before TLS and HTTP parsing. The connection owns
// its slot through hijacking, until its first Close. Both gateway compositions
// must wrap their raw TCP listener before the TLS listener.
func (g *Gateway) LimitListener(listener net.Listener) net.Listener {
	return &admissionListener{Listener: listener, slots: g.connections}
}

type admissionListener struct {
	net.Listener
	slots chan struct{}
}

func (l *admissionListener) Accept() (net.Conn, error) {
	for {
		connection, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if acquire(l.slots) {
			return &admittedConnection{Conn: connection, slots: l.slots}, nil
		}
		_ = connection.Close()
	}
}

type admittedConnection struct {
	net.Conn
	slots chan struct{}
	once  sync.Once
}

func (c *admittedConnection) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { <-c.slots })
	return err
}

func (g *Gateway) acquireSession(execution gateway.Reference) bool {
	g.sessionsMu.Lock()
	defer g.sessionsMu.Unlock()
	if g.sessions[execution] >= g.config.MaxSessionsPerExecution {
		return false
	}
	g.sessions[execution]++
	return true
}

func (g *Gateway) releaseSession(execution gateway.Reference) {
	g.sessionsMu.Lock()
	defer g.sessionsMu.Unlock()
	g.sessions[execution]--
	if g.sessions[execution] == 0 {
		delete(g.sessions, execution)
	}
}

// authorityCall fails closed on deadline even if a dependency ignores context.
// Its worker retains admission until it actually returns: cancellation is not
// evidence that dependency work stopped, nor permission for unbounded workers.
func authorityCall[T any](parent context.Context, g *Gateway, call func(context.Context) (T, error)) (T, error) {
	var zero T
	ctx, cancel := context.WithTimeout(parent, g.config.AuthorityTimeout)
	defer cancel()
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	if !acquire(g.authorityCalls) {
		return zero, fmt.Errorf("gateway authority capacity unavailable")
	}
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, err := call(ctx)
		// Relinquish the completed lookup before its caller starts the next one.
		<-g.authorityCalls
		done <- result{value, err}
	}()
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case result := <-done:
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		return result.value, result.err
	}
}

func (g *Gateway) revalidate(ctx context.Context, identity *gateway.ExecutionGrant) error {
	_, err := authorityCall(ctx, g, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, g.config.Revalidate(ctx, identity)
	})
	return err
}

func minDeadline(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
