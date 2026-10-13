package session

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/google/uuid"
)

// Create imports a trusted source's first stopped native checkpoint. Repeated
// imports must carry exactly the same intent, receipt and artifact digest.
func (c *Coordinator) Create(ctx context.Context, start Start) (Record, error) {
	unlock, err := c.lockSession(start.Source.SessionID)
	if err != nil {
		return Record{}, err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	source := start.Source
	source.Deadline = source.Deadline.UTC()
	if err := validateLaunch(source); err != nil {
		return Record{}, err
	}
	if _, ok := c.placements[source.Placement]; !ok {
		return Record{}, errors.New("session: source placement unavailable")
	}
	if !validIncarnation(start.Incarnation) {
		return Record{}, errors.New("session: source incarnation required")
	}
	if err := validateStop(source, start.Incarnation, start.Stopped); err != nil {
		return Record{}, err
	}
	existing, err := c.load(source.SessionID)
	if err == nil {
		if _, err = c.commitCheckpoint(start.Stopped.Checkpoint, source.Harness, source.ThreadID, start.Stopped.CheckpointDigest); err != nil {
			return Record{}, err
		}
		first := existing.Visits[0]
		if !sameLaunch(first.Launch, source) || first.Incarnation != start.Incarnation || !sameReceipt(first.Receipt, start.Stopped.Receipt) || existing.Visits[0].Receipt == nil || existing.Visits[0].Receipt.Launch.CheckpointDigest != source.CheckpointDigest {
			return Record{}, errors.New("session: import intent conflicts with retained session")
		}
		// Import retries never replace a later committed checkpoint.
		if start.Stopped.CheckpointDigest != checkpointDigestForSeed(existing) {
			return Record{}, errors.New("session: import checkpoint conflicts")
		}
		return existing, nil
	}
	if !os.IsNotExist(err) {
		return Record{}, err
	}
	if err = beforeDeadline(source); err != nil {
		return Record{}, err
	}
	cp, err := c.commitCheckpoint(start.Stopped.Checkpoint, source.Harness, source.ThreadID, start.Stopped.CheckpointDigest)
	if err != nil {
		return Record{}, err
	}
	remaining, err := subtract(source.Allowances, *start.Stopped.Receipt)
	if err != nil {
		return Record{}, err
	}
	r := Record{Version: journalVersion, ID: source.SessionID, Harness: source.Harness, ThreadID: source.ThreadID, InstructionDigest: source.InstructionDigest, IncludeIgnored: append([]string(nil), source.IncludeIgnored...), Deadline: source.Deadline, Ceiling: cloneAllowances(source.Allowances), Remaining: remaining, Checkpoint: cp, Phase: PhaseIdle, Epoch: source.Epoch, Visits: []Visit{{Launch: source, Incarnation: start.Incarnation, Receipt: start.Stopped.Receipt, ActualStopped: true}}}
	// Retain the initial checkpoint identity in the source visit rather than infer
	// it from a later checkpoint when replaying Create.
	r.Visits[0].CheckpointDigest = cp.Metadata.SHA256
	return r, c.save(r)
}
func sameReceipt(a, b *Receipt) bool {
	if a == nil || b == nil {
		return a == b
	}
	if !sameLaunch(a.Launch, b.Launch) || a.Incarnation != b.Incarnation || len(a.Usage) != len(b.Usage) {
		return false
	}
	for k, v := range a.Usage {
		if value, ok := b.Usage[k]; !ok || value != v {
			return false
		}
	}
	return true
}
func checkpointDigestForSeed(r Record) string { return r.Visits[0].CheckpointDigest }

// Inspect reads a durable snapshot; it does not turn backend unavailability into
// a new placement, stopped source, zero usage or refunded capacity.
func (c *Coordinator) Inspect(id string) (Record, error) {
	unlock, err := c.lockSession(id)
	if err != nil {
		return Record{}, err
	}
	defer unlock()
	return c.load(id)
}

// Move owns all stages and retries. Unknown stop/usage keeps the source allocation
// encumbered and blocks even destination preparation.
func (c *Coordinator) Move(ctx context.Context, id, operationID, destination string) (Record, error) {
	unlock, err := c.lockSession(id)
	if err != nil {
		return Record{}, err
	}
	defer unlock()
	if !validUUID(operationID) {
		return Record{}, errors.New("session: invalid operation ID")
	}
	if _, ok := c.placements[destination]; !ok {
		return Record{}, errors.New("session: destination unavailable")
	}
	r, err := c.load(id)
	if err != nil {
		return r, err
	}
	if r.CancelOperation != "" {
		return r, errors.New("session: session cancelled")
	}
	ctx, finish, err := c.graceful.Begin(ctx, id)
	if err != nil {
		return r, err
	}
	defer finish()
	for i := range r.Operations {
		op := &r.Operations[i]
		if op.ID == operationID {
			if op.Finalize || op.Destination != destination {
				return r, errors.New("session: operation intent conflict")
			}
			if op.Complete {
				return r, nil
			}
			return c.drive(ctx, r, i)
		}
	}
	if r.PendingOperation != "" {
		return r, errors.New("session: previous move requires recovery")
	}
	for _, v := range r.Visits {
		if v.Launch.OperationID == operationID {
			return r, errors.New("session: operation ID reused")
		}
	}
	if len(r.Operations) >= maxOperations {
		return r, errors.New("session: operation retention limit reached")
	}
	stage := stagePrepare
	r.Phase = PhasePreparing
	if r.Active != nil {
		stage = stageStop
		r.Phase = PhaseStopping
	}
	r.Operations = append(r.Operations, Operation{ID: operationID, Destination: destination, Stage: stage})
	r.PendingOperation = operationID
	if err = c.save(r); err != nil {
		return r, err
	}
	return c.drive(ctx, r, len(r.Operations)-1)
}

// Recover resumes the same journaled operation/allocation after a lost response or
// process restart. It never silently generates a replacement allocation.
func (c *Coordinator) Recover(ctx context.Context, id string) (Record, error) {
	unlock, err := c.lockSession(id)
	if err != nil {
		return Record{}, err
	}
	defer unlock()
	r, err := c.load(id)
	if err != nil {
		return r, err
	}
	if r.CancelOperation != "" {
		return c.abort(ctx, r)
	}
	if r.PendingOperation == "" {
		return r, nil
	}
	ctx, finish, err := c.graceful.Begin(ctx, id)
	if err != nil {
		return r, err
	}
	defer finish()
	for i := range r.Operations {
		if r.Operations[i].ID == r.PendingOperation {
			return c.drive(ctx, r, i)
		}
	}
	return r, errors.New("session: pending operation missing")
}
func (c *Coordinator) drive(ctx context.Context, r Record, index int) (Record, error) {
	if err := ctx.Err(); err != nil {
		return r, err
	}
	var err error
	r, err = c.stopSource(ctx, r, index)
	if err != nil {
		return r, err
	}
	if r.Operations[index].Finalize {
		return c.finishFinalization(r, index)
	}
	if err = ctx.Err(); err != nil {
		return r, err
	}
	r, err = c.prepareDestination(ctx, r, index)
	if err != nil {
		return r, err
	}
	if err = ctx.Err(); err != nil {
		return r, err
	}
	return c.activateDestination(ctx, r, index)
}
func (c *Coordinator) stopSource(ctx context.Context, r Record, index int) (Record, error) {
	op := &r.Operations[index]
	if op.Stage == stageStop {
		if r.Active == nil {
			return r, errors.New("session: stop intent lacks active allocation")
		}
		source := r.Active
		p, ok := c.placements[source.Launch.Placement]
		if !ok {
			return r, errors.New("session: source placement unavailable")
		}
		stopped, err := p.Stop(ctx, source.Launch, source.Incarnation)
		if err != nil {
			r.Phase = PhaseUnknownStop
			return r, errors.Join(err, c.save(r))
		}
		if !stopped.ActualStopped || !sameLaunch(source.Launch, stopped.Launch) || source.Incarnation != stopped.Incarnation {
			r.Phase = PhaseUnknownStop
			return r, errors.Join(errors.New("session: source stop remains unknown"), c.save(r))
		}
		source.ActualStopped = true
		if err = validateReceipt(source.Launch, source.Incarnation, stopped.Receipt); err != nil {
			r.Phase = PhaseUnknownUsage
			return r, errors.Join(err, c.save(r))
		}
		cp, err := c.commitCheckpoint(stopped.Checkpoint, r.Harness, r.ThreadID, stopped.CheckpointDigest)
		if err != nil {
			return r, err
		}
		remaining, err := subtract(r.Remaining, *stopped.Receipt)
		if err != nil {
			r.Phase = PhaseUnknownUsage
			return r, errors.Join(err, c.save(r))
		}
		r.Visits = append(r.Visits, Visit{Launch: source.Launch, Incarnation: source.Incarnation, Receipt: stopped.Receipt, ActualStopped: true, CheckpointDigest: cp.Metadata.SHA256})
		r.Remaining = remaining
		r.Checkpoint = cp
		r.Active = nil
		op.Stage = stagePrepare
		r.Phase = PhasePreparing
		if err = c.save(r); err != nil {
			return r, err
		}
	}
	return r, nil
}
func (c *Coordinator) prepareDestination(ctx context.Context, r Record, index int) (Record, error) {
	op := &r.Operations[index]
	if op.Stage == stagePrepare {
		if op.Launch == nil {
			if r.Epoch == ^uint64(0) {
				return r, errors.New("session: epoch exhausted")
			}
			launch := Launch{Harness: r.Harness, SessionID: r.ID, OperationID: op.ID, AllocationID: uuid.NewString(), Epoch: r.Epoch + 1, Placement: op.Destination, ThreadID: r.ThreadID, InstructionDigest: r.InstructionDigest, IncludeIgnored: append([]string(nil), r.IncludeIgnored...), Deadline: r.Deadline, Allowances: cloneAllowances(r.Remaining), CheckpointDigest: r.Checkpoint.Metadata.SHA256}
			op.Launch = &launch
			if err := c.save(r); err != nil {
				return r, err
			}
		}
		if err := beforeDeadline(*op.Launch); err != nil {
			return r, err
		}
		if err := spendable(op.Launch.Allowances); err != nil {
			return r, err
		}
		p, ok := c.placements[op.Destination]
		if !ok {
			return r, errors.New("session: destination placement unavailable")
		}
		observation, err := p.Inspect(ctx, *op.Launch)
		if err != nil {
			return r, err
		}
		if err = checkObservation(*op.Launch, observation); err != nil {
			return r, err
		}
		var incarnation Incarnation
		switch observation.State {
		case PlacementAbsent, PlacementPreparing:
			if err := ctx.Err(); err != nil {
				return r, err
			}
			data, err := c.checkpointBytes(r)
			if err != nil {
				return r, err
			}
			if err = ctx.Err(); err != nil {
				return r, err
			}
			incarnation, err = p.Prepare(ctx, *op.Launch, data)
			if err != nil {
				return r, err
			}
		case PlacementPrepared:
			incarnation = observation.Incarnation
		default:
			return r, fmt.Errorf("session: preparation state remains %s", observation.State)
		}
		if !validIncarnation(incarnation) {
			return r, errors.New("session: invalid prepared incarnation")
		}
		op.Incarnation = &incarnation
		op.Stage = stageActivate
		r.Phase = PhaseActivating
		if err = c.save(r); err != nil {
			return r, err
		}
	}
	return r, nil
}
func (c *Coordinator) activateDestination(ctx context.Context, r Record, index int) (Record, error) {
	op := &r.Operations[index]
	if op.Stage == stageActivate {
		p, ok := c.placements[op.Destination]
		if !ok {
			return r, errors.New("session: destination placement unavailable")
		}
		observation, err := p.Inspect(ctx, *op.Launch)
		if err != nil {
			return r, err
		}
		if err = checkObservation(*op.Launch, observation); err != nil {
			return r, err
		}
		if observation.Incarnation != *op.Incarnation {
			return r, errors.New("session: destination incarnation changed")
		}
		switch observation.State {
		case PlacementPrepared:
			if err = ctx.Err(); err != nil {
				return r, err
			}
			if err = beforeDeadline(*op.Launch); err != nil {
				return r, err
			}
			if err = p.Activate(ctx, *op.Launch, *op.Incarnation); err != nil {
				return r, err
			}
		case PlacementActive: // The original activation succeeded but its response was lost.
		default:
			return r, fmt.Errorf("session: activation state remains %s", observation.State)
		}
		r.Active = &Visit{Launch: *op.Launch, Incarnation: *op.Incarnation}
		r.Epoch = op.Launch.Epoch
		r.Phase = PhaseActive
		op.Complete = true
		op.Stage = stageComplete
		r.PendingOperation = ""
		return r, c.save(r)
	}
	return r, nil
}
func checkObservation(l Launch, o Observation) error {
	if !sameLaunch(l, o.Launch) {
		return errors.New("session: observation operation/allocation mismatch")
	}
	switch o.State {
	case PlacementAbsent, PlacementUnknown:
	case PlacementPreparing:
		if o.Incarnation != (Incarnation{}) && !validIncarnation(o.Incarnation) {
			return errors.New("session: observation has partial incarnation")
		}
	case PlacementPrepared, PlacementActive, PlacementStopped:
		if !validIncarnation(o.Incarnation) {
			return errors.New("session: observation missing incarnation")
		}
	default:
		return errors.New("session: invalid placement state")
	}
	return nil
}
