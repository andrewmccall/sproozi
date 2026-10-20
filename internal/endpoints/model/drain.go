package model

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/proxytransport"
)

// errGatewayStopping is the cause of a model call ended by Drain.Shutdown.
var errGatewayStopping = errors.New("gateway is shutting down")

// recoveryMargin is how long after its deadline an abandoned call's
// reservation is recovered: past its settlement and any clock skew.
const recoveryMargin = 5 * time.Minute

// Drain lets a stopping gateway settle its model calls. Shutdown refuses new
// calls, ends the ones in flight as interrupted and waits for each one to
// settle its reservation.
type Drain struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	mu     sync.Mutex
	calls  int
	idle   chan struct{}
}

func NewDrain() *Drain {
	ctx, cancel := context.WithCancelCause(context.Background())
	return &Drain{ctx: ctx, cancel: cancel}
}

// Stopping reports whether Shutdown has begun.
func (d *Drain) Stopping() bool { return d != nil && d.ctx.Err() != nil }

// enter admits one call, or reports that the gateway is stopping.
func (d *Drain) enter() (func(), bool) {
	if d == nil {
		return func() {}, true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx.Err() != nil {
		return nil, false
	}
	d.calls++
	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.calls--
			if d.calls == 0 && d.idle != nil {
				close(d.idle)
				d.idle = nil
			}
		})
	}, true
}

// Shutdown ends every model call and waits until each has settled, or ctx ends.
func (d *Drain) Shutdown(ctx context.Context) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	d.cancel(errGatewayStopping)
	if d.calls == 0 {
		d.mu.Unlock()
		return nil
	}
	if d.idle == nil {
		d.idle = make(chan struct{})
	}
	idle := d.idle
	d.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// recovery is the charge for this call if its gateway stops without settling
// it: the request bytes ÷ 4 for input and its whole maximum duration at R for
// output, capped at its output bound. The ledger caps it at the reservation.
// A request whose input cannot be sized is charged the whole reservation.
func (c *modelCall) recovery() *budget.Recovery {
	now := time.Now()
	deadline, ok := c.ctx.Deadline()
	if !ok {
		deadline = now.Add(proxytransport.MaxModelCallDurationLimit)
	}
	if !c.plan.sizable {
		return &budget.Recovery{ExpiresAt: deadline.Add(recoveryMargin), Units: c.recoveryCap(c.limits.MaxUnits),
			CostMicros: c.recoveryCap(c.limits.MaxCostMicros)}
	}
	rate := c.h.Config.OutputTokensPerSecond
	if rate <= 0 {
		rate = DefaultOutputTokensPerSecond
	}
	output := ceilDiv(max(deadline.Sub(now), 0).Nanoseconds(), int64(time.Second), rate)
	if c.plan.outputBound > 0 {
		output = min(output, c.plan.outputBound)
	}
	units, cost, _ := c.usage(output)
	return &budget.Recovery{ExpiresAt: deadline.Add(recoveryMargin), Units: units, CostMicros: cost}
}

// recoveryCap charges a limited dimension's whole reservation.
func (c *modelCall) recoveryCap(limit int64) int64 {
	if limit > 0 {
		return math.MaxInt64
	}
	return 0
}
