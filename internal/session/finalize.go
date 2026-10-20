package session

import (
	"context"
	"errors"
)

// Finalize retains an actual stopped writer, sealed usage and checkpoint without
// activating a destination. Its durable operation uses the ordinary stop/recovery
// path, so a lost reply cannot turn finalization into another placement visit.
func (c *Coordinator) Finalize(ctx context.Context, id, operationID string) (Record, error) {
	unlock, err := c.lockSession(id)
	if err != nil {
		return Record{}, err
	}
	defer unlock()
	if !validUUID(operationID) {
		return Record{}, errors.New("session: invalid finalization operation")
	}
	r, err := c.load(id)
	if err != nil {
		return r, err
	}
	if r.CancelOperation != "" {
		return r, cancelled(r)
	}
	ctx, finish, err := c.graceful.Begin(ctx, id)
	if err != nil {
		return r, err
	}
	defer finish()
	for i, op := range r.Operations {
		if op.ID == operationID {
			if !op.Finalize {
				return r, errors.New("session: operation intent conflict")
			}
			if op.done() {
				return r, nil
			}
			return c.drive(ctx, r, i)
		}
	}
	if r.PendingOperation != "" {
		return r, pendingRecovery(r)
	}
	for _, visit := range r.Visits {
		if visit.Launch.OperationID == operationID {
			return r, errors.New("session: operation ID reused")
		}
	}
	if len(r.Operations) >= maxOperations {
		return r, errors.New("session: operation retention limit reached")
	}
	if r.Active == nil {
		// Nothing runs: retain the operation binding so a replay stays idle.
		r.Operations = append(r.Operations, Operation{ID: operationID, Finalize: true, Stage: stageComplete})
		return r, c.save(r)
	}
	r.Operations = append(r.Operations, Operation{ID: operationID, Finalize: true, Stage: stageStop})
	r.PendingOperation = operationID
	r.Phase = PhaseStopping
	if err = c.save(r); err != nil {
		return r, err
	}
	return c.drive(ctx, r, len(r.Operations)-1)
}

func (c *Coordinator) finishFinalization(r Record, index int) (Record, error) {
	if r.Active != nil || r.Operations[index].Stage != stagePrepare {
		return r, errors.New("session: finalization stop remains unresolved")
	}
	r.Operations[index].Stage = stageComplete
	r.PendingOperation = ""
	r.Phase = PhaseIdle
	return r, c.save(r)
}
