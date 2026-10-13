package session

import (
	"context"
	"errors"
)

// Cancel durably prevents activation before asking a placement to revoke and
// forcibly stop. Unknown stop or usage never refunds the encumbered allocation.
// Unlike a move, cancellation can preserve the last checkpoint without a new one.
func (c *Coordinator) Cancel(ctx context.Context, id, operationID string) (Record, error) {
	if !validUUID(id) || !validUUID(operationID) {
		return Record{}, errors.New("session: invalid session or cancellation ID")
	}
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	finish, err := c.graceful.RequestCancel(id, operationID)
	if err != nil {
		return Record{}, err
	}
	defer finish()
	unlock, err := c.lockSession(id)
	if err != nil {
		return Record{}, err
	}
	defer unlock()
	r, err := c.load(id)
	if err != nil {
		return r, err
	}
	if r.CancelOperation != "" && r.CancelOperation != operationID {
		return r, errors.New("session: cancellation intent conflict")
	}
	if r.CancelOperation == "" {
		r.CancelOperation = operationID
		r.Phase = PhaseCancelled
		if err = c.save(r); err != nil {
			return r, err
		}
	}
	return c.abort(ctx, r)
}
func (c *Coordinator) abort(ctx context.Context, r Record) (Record, error) {
	active := r.Active
	if active == nil && r.PendingOperation != "" {
		for i := range r.Operations {
			op := &r.Operations[i]
			if op.ID != r.PendingOperation || op.Launch == nil {
				continue
			}
			p, ok := c.placements[op.Destination]
			if !ok {
				return r, errors.New("session: pending placement unavailable")
			}
			o, err := p.Inspect(ctx, *op.Launch)
			if err != nil {
				r.Uncertainty = PhaseUnknownStop
				return r, errors.Join(err, c.save(r))
			}
			if err = checkObservation(*op.Launch, o); err != nil {
				return r, err
			}
			if o.State == PlacementAbsent {
				op.Complete = true
				op.Stage = stageCancelled
				r.PendingOperation = ""
				r.Uncertainty = ""
				return r, c.save(r)
			}
			if !validIncarnation(o.Incarnation) {
				r.Uncertainty = PhaseUnknownStop
				return r, errors.Join(errors.New("session: cancelled preparation remains unknown"), c.save(r))
			}
			if op.Incarnation != nil && o.Incarnation != *op.Incarnation {
				return r, errors.New("session: cancelled preparation incarnation changed")
			}
			active = &Visit{Launch: *op.Launch, Incarnation: o.Incarnation}
			r.Active = active
			r.Epoch = active.Launch.Epoch
		}
	}
	if active == nil {
		return r, nil
	}
	p, ok := c.placements[active.Launch.Placement]
	if !ok {
		return r, errors.New("session: active placement unavailable")
	}
	o, err := p.Abort(ctx, active.Launch, active.Incarnation)
	if err != nil {
		r.Uncertainty = PhaseUnknownStop
		return r, errors.Join(err, c.save(r))
	}
	if err = checkObservation(active.Launch, o); err != nil {
		return r, err
	}
	if o.Incarnation != active.Incarnation || o.State != PlacementStopped {
		r.Uncertainty = PhaseUnknownStop
		return r, errors.Join(errors.New("session: abort did not prove the source stopped"), c.save(r))
	}
	active.ActualStopped = true
	if o.Stopped == nil || !o.Stopped.ActualStopped || validateStop(active.Launch, active.Incarnation, *o.Stopped) != nil {
		r.Uncertainty = PhaseUnknownUsage
		return r, c.save(r)
	}
	remaining, err := subtract(r.Remaining, *o.Stopped.Receipt)
	if err != nil {
		return r, err
	}
	cp := r.Checkpoint
	if len(o.Stopped.Checkpoint) > 0 {
		cp, err = c.commitCheckpoint(o.Stopped.Checkpoint, r.Harness, r.ThreadID, o.Stopped.CheckpointDigest)
		if err != nil {
			return r, err
		}
	}
	r.Visits = append(r.Visits, Visit{Launch: active.Launch, Incarnation: active.Incarnation, ActualStopped: true, Receipt: o.Stopped.Receipt, CheckpointDigest: cp.Metadata.SHA256})
	r.Remaining = remaining
	r.Checkpoint = cp
	r.Active = nil
	r.Uncertainty = ""
	for i := range r.Operations {
		if r.Operations[i].ID == r.PendingOperation {
			r.Operations[i].Complete = true
			r.Operations[i].Stage = stageCancelled
		}
	}
	r.PendingOperation = ""
	return r, c.save(r)
}
