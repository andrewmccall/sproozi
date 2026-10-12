// Package lifecycle interrupts graceful operations without allowing concurrent
// journal mutations. Callers keep their existing lifecycle mutex for all writes.
package lifecycle

import (
	"context"
	"errors"
	"sync"
)

type cancellation struct {
	operation string
	callers   int
}

type flight struct {
	cancel context.CancelFunc
}

// Control's zero value is ready. Begin is called while the journal mutex is
// held; RequestCancel is called before acquiring it. Pending cancellation blocks
// a queued graceful operation from slipping in before cancellation is persisted.
type Control struct {
	mu      sync.Mutex
	flights map[string]*flight
	pending map[string]*cancellation
}

func (c *Control) Begin(ctx context.Context, id string) (context.Context, func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if c.pending[id] != nil {
		return nil, nil, context.Canceled
	}
	if c.flights[id] != nil {
		return nil, nil, errors.New("session: graceful operation already running")
	}
	if c.flights == nil {
		c.flights = make(map[string]*flight)
	}
	run, cancel := context.WithCancel(ctx)
	active := &flight{cancel: cancel}
	c.flights[id] = active
	return run, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.flights[id] == active {
			delete(c.flights, id)
		}
		cancel()
	}, nil
}

// RequestCancel requires IDs already validated at the public boundary. The
// ticket holds no authority and is removed after the serialized Cancel call:
// retained cancellation identity and stopped/usage evidence stay in the journal.
func (c *Control) RequestCancel(id, operation string) (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ticket := c.pending[id]
	if ticket != nil && ticket.operation != operation {
		return nil, errors.New("session: cancellation intent conflict")
	}
	if ticket == nil {
		ticket = &cancellation{operation: operation}
		if c.pending == nil {
			c.pending = make(map[string]*cancellation)
		}
		c.pending[id] = ticket
	}
	ticket.callers++
	if active := c.flights[id]; active != nil {
		active.cancel()
	}
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		ticket.callers--
		if ticket.callers == 0 {
			delete(c.pending, id)
		}
	}, nil
}
