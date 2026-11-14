package session

import (
	"context"
	"errors"
	"fmt"
)

// Cancel durably prevents activation before asking a placement to revoke and
// forcibly stop. Unknown stop or usage never refunds the encumbered allocation.
// Unlike a move, cancellation can preserve the last checkpoint without a new one.
//
// Cancel first interrupts any graceful operation on the execution, so it never
// waits for a native turn. admit, when non-nil, runs next and before the intent
// is persisted: an owner uses it to take its own lock and bind the request, and
// the returned release runs when Cancel returns.
func (c *Coordinator) Cancel(ctx context.Context, id, operationID string, admit func() (func(), error)) (Record, error) {
	return c.cancel(ctx, id, operationID, admit, evCancel)
}

// ErrAlreadyStopped refuses CancelUnsettled for an execution that already
// stopped, with its usage sealed, before the cancellation was retained.
var ErrAlreadyStopped = errors.New("session: execution already stopped; nothing to cancel")

// CancelUnsettled is Cancel for an owner that keeps a stopped execution's
// result, such as a remote task's answer: it refuses an idle execution with
// ErrAlreadyStopped. It decides under the execution's lock, so a stop that
// commits while the cancellation waits for that lock wins, and the result is
// kept.
func (c *Coordinator) CancelUnsettled(ctx context.Context, id, operationID string, admit func() (func(), error)) (Record, error) {
	return c.cancel(ctx, id, operationID, admit, evCancelUnsettled)
}

func (c *Coordinator) cancel(ctx context.Context, id, operationID string, admit func() (func(), error), ev event) (Record, error) {
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
	if admit != nil {
		release, err := admit()
		if err != nil {
			return Record{}, err
		}
		defer release()
	}
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
		// Another cancellation already owns this execution; recover finishes it.
		return r, fmt.Errorf("session: cancellation %s was already requested: %w", r.CancelOperation, cancelled(r))
	}
	if r.CancelOperation == "" {
		if ev == evCancelUnsettled && r.Phase == PhaseIdle {
			return r, ErrAlreadyStopped
		}
		r.CancelOperation = operationID
		if err = c.commit(&r, ev); err != nil {
			return r, err
		}
	}
	return c.abort(ctx, r)
}

// abort carries out retained cancellation. It forcibly stops the active writer,
// or a pending destination that may have started, and ends in PhaseCancelled
// only once no writer can run and its usage is sealed.
func (c *Coordinator) abort(ctx context.Context, r Record) (Record, error) {
	if r.Phase == PhaseCancelled {
		return r, nil
	}
	if r.Active == nil && r.PendingOperation != "" {
		var resolved bool
		var err error
		r, resolved, err = c.claimPendingWriter(ctx, r)
		if err != nil || resolved {
			return r, err
		}
	}
	if r.Active != nil {
		var err error
		if r, err = c.abortActive(ctx, r); err != nil || r.Active != nil {
			return r, err
		}
	}
	if r.PendingOperation != "" {
		r.Operations[len(r.Operations)-1].Stage = stageCancelled
		r.PendingOperation = ""
	}
	return r, c.commit(&r, evCancelled)
}

// claimPendingWriter makes a pending destination the active visit so that it
// can be aborted. A known prepared incarnation is used directly: inspecting it
// could replay an interrupted activation. A placement that can fence an
// unreported preparation does so without inspecting it. resolved reports that
// no writer can exist, so cancellation is complete.
func (c *Coordinator) claimPendingWriter(ctx context.Context, r Record) (Record, bool, error) {
	op := &r.Operations[len(r.Operations)-1]
	if op.Launch == nil {
		return r, false, nil
	}
	p, ok := c.placement(op.Destination)
	if !ok {
		return r, false, errors.New("session: pending placement unavailable")
	}
	var incarnation Incarnation
	aborter, fencesPreparation := p.(PreparationAborter)
	switch {
	case op.Incarnation != nil:
		incarnation = *op.Incarnation
	case fencesPreparation:
		if err := aborter.AbortPreparation(ctx, *op.Launch); err != nil {
			return r, false, errors.Join(err, c.commit(&r, evStopUnproven))
		}
		op.Stage = stageCancelled
		r.PendingOperation = ""
		return r, true, c.commit(&r, evCancelled)
	default:
		o, err := p.Inspect(ctx, *op.Launch)
		if err != nil {
			return r, false, errors.Join(err, c.commit(&r, evStopUnproven))
		}
		if err = checkObservation(*op.Launch, o); err != nil {
			return r, false, err
		}
		if o.State == PlacementAbsent {
			op.Stage = stageCancelled
			r.PendingOperation = ""
			return r, true, c.commit(&r, evCancelled)
		}
		if !validIncarnation(o.Incarnation) {
			return r, false, errors.Join(errors.New("session: cancelled preparation remains unknown"), c.commit(&r, evStopUnproven))
		}
		incarnation = o.Incarnation
	}
	r.Active = &Visit{Launch: *op.Launch, Incarnation: incarnation}
	r.Epoch = op.Launch.Epoch
	return r, false, nil
}

// abortActive revokes and fences the active writer, then retires the visit
// with its sealed receipt. Unproven stop or usage leaves it active.
func (c *Coordinator) abortActive(ctx context.Context, r Record) (Record, error) {
	active := r.Active
	p, ok := c.placement(active.Launch.Placement)
	if !ok {
		return r, errors.New("session: active placement unavailable")
	}
	o, err := p.Abort(ctx, active.Launch, active.Incarnation)
	if err != nil {
		return r, errors.Join(err, c.commit(&r, evStopUnproven))
	}
	if err = checkObservation(active.Launch, o); err != nil {
		return r, err
	}
	if o.Incarnation != active.Incarnation || o.State != PlacementStopped {
		return r, errors.Join(errors.New("session: abort did not prove the source stopped"), c.commit(&r, evStopUnproven))
	}
	active.ActualStopped = true
	if o.Stopped == nil || !o.Stopped.ActualStopped || validateStop(active.Launch, active.Incarnation, *o.Stopped) != nil {
		return r, c.commit(&r, evUsageUnsealed)
	}
	remaining, err := subtract(r.Remaining, *o.Stopped.Receipt)
	if err != nil {
		return r, err
	}
	thread, cp, err := c.commitStopped(r, *o.Stopped)
	if err != nil {
		return r, err
	}
	r.Visits = append(r.Visits, Visit{Launch: active.Launch, Incarnation: active.Incarnation, ActualStopped: true, Receipt: o.Stopped.Receipt, CheckpointDigest: cp.Metadata.SHA256})
	r.Remaining = remaining
	r.ThreadID, r.Checkpoint = thread, cp
	r.Active = nil
	return r, nil
}
