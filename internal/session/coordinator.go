package session

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/google/uuid"
)

// Create retains a new execution and its first visit without starting it;
// Recover then prepares and activates that visit like any other destination.
// first is the epoch-1 launch whose ID becomes the execution ID. input is the
// checkpoint named by first.CheckpointDigest, or a captured Git workspace when
// the launch has no checkpoint or thread. session names the owning durable
// session when it differs from the execution. A retry must repeat the same
// launch and input; it returns the retained execution unchanged.
func (c *Coordinator) Create(first Launch, input []byte, session string) (Record, error) {
	unlock, err := c.lockSession(first.ExecutionID)
	if err != nil {
		return Record{}, err
	}
	defer unlock()
	first.Deadline = first.Deadline.UTC()
	if err = validateLaunch(first); err != nil {
		return Record{}, err
	}
	if first.Epoch != 1 || session != "" && (!validUUID(session) || session == first.ExecutionID) {
		return Record{}, errors.New("session: invalid first visit")
	}
	if _, ok := c.placements[first.Placement]; !ok {
		return Record{}, errors.New("session: first placement unavailable")
	}
	existing, err := c.load(first.ExecutionID)
	if err == nil {
		if !SameLaunch(*existing.Operations[0].Launch, first) || existing.Session != session {
			return existing, errors.New("session: first visit conflicts with retained execution")
		}
		if retained, err := c.input(existing, first); err != nil || string(retained) != string(input) {
			return existing, errors.Join(errors.New("session: first visit input conflicts"), err)
		}
		return existing, nil
	}
	if !os.IsNotExist(err) {
		return Record{}, err
	}
	if err = c.beforeDeadline(first); err != nil {
		return Record{}, err
	}
	r := Record{Version: journalVersion, ID: first.ExecutionID, Session: session, Harness: first.Harness,
		ThreadID: first.ThreadID, InstructionDigest: first.InstructionDigest,
		IncludeIgnored: append([]string(nil), first.IncludeIgnored...), Deadline: first.Deadline,
		Ceiling: cloneAllowances(first.Allowances), Remaining: cloneAllowances(first.Allowances),
		Phase: PhasePreparing, PendingOperation: first.OperationID,
		Operations: []Operation{{ID: first.OperationID, Destination: first.Placement, Launch: &first, Stage: stagePrepare}}}
	if first.CheckpointDigest == "" {
		r.Workspace, err = c.retainWorkspace(input)
	} else {
		r.Checkpoint, err = c.commitCheckpoint(input, first.Harness, first.ThreadID, first.CheckpointDigest)
	}
	if err != nil {
		return Record{}, err
	}
	return r, c.save(r)
}

// Inspect reads the last durable snapshot; it does not turn backend
// unavailability into a new placement, stopped source, zero usage or refunded
// capacity. Journals are replaced atomically, so Inspect does not wait for an
// operation in progress, such as a move waiting for a native turn.
func (c *Coordinator) Inspect(id string) (Record, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.ready(); err != nil {
		return Record{}, err
	}
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
		return r, cancelled(r)
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
			if op.done() {
				return r, nil
			}
			return c.drive(ctx, r, i)
		}
	}
	if r.PendingOperation != "" {
		return r, pendingRecovery(r)
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
		fenced := fencedBy(source.Launch, source.Incarnation, stopped)
		if err != nil && (!errors.Is(err, ErrUsageUnsealed) || !fenced) {
			r.Phase = PhaseUnknownStop
			return r, errors.Join(err, c.save(r))
		}
		if !fenced {
			r.Phase = PhaseUnknownStop
			return r, errors.Join(errors.New("session: source stop remains unknown"), c.save(r))
		}
		source.ActualStopped = true
		if err != nil {
			r.Phase = PhaseUnknownUsage
			return r, errors.Join(err, c.save(r))
		}
		if err = validateReceipt(source.Launch, source.Incarnation, stopped.Receipt); err != nil {
			r.Phase = PhaseUnknownUsage
			return r, errors.Join(err, c.save(r))
		}
		if len(stopped.Checkpoint) == 0 {
			return r, errors.New("session: graceful stop returned no checkpoint")
		}
		thread, cp, err := c.commitStopped(r, stopped)
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
		r.ThreadID, r.Checkpoint = thread, cp
		r.Active = nil
		op.Stage = stagePrepare
		r.Phase = PhasePreparing
		if err = c.save(r); err != nil {
			return r, err
		}
	}
	return r, nil
}

// commitStopped commits a stop's checkpoint. The first one also binds the
// execution's genuine native thread; later ones must name the same thread.
// A stop without a checkpoint keeps the previous one.
func (c *Coordinator) commitStopped(r Record, s Stopped) (string, Checkpoint, error) {
	if len(s.Checkpoint) == 0 {
		return r.ThreadID, r.Checkpoint, nil
	}
	observed := stoppedThread(s)
	thread := cmp.Or(r.ThreadID, observed)
	if !validUUID(thread) || observed != thread {
		return "", Checkpoint{}, errors.New("session: stopped native thread conflicts with the execution")
	}
	cp, err := c.commitCheckpoint(s.Checkpoint, r.Harness, thread, s.CheckpointDigest)
	return thread, cp, err
}
func (c *Coordinator) prepareDestination(ctx context.Context, r Record, index int) (Record, error) {
	op := &r.Operations[index]
	if op.Stage == stagePrepare {
		if op.Launch == nil {
			if r.Epoch == ^uint64(0) {
				return r, errors.New("session: epoch exhausted")
			}
			if !r.hasCheckpoint() {
				return r, errors.New("session: execution has no checkpoint to move")
			}
			launch := Launch{Harness: r.Harness, ExecutionID: r.ID, OperationID: op.ID, AllocationID: uuid.NewString(), Epoch: r.Epoch + 1, Placement: op.Destination, ThreadID: r.ThreadID, InstructionDigest: r.InstructionDigest, IncludeIgnored: append([]string(nil), r.IncludeIgnored...), Deadline: r.Deadline, Allowances: cloneAllowances(r.Remaining), CheckpointDigest: r.Checkpoint.Metadata.SHA256}
			op.Launch = &launch
			if err := c.save(r); err != nil {
				return r, err
			}
		}
		if err := c.beforeDeadline(*op.Launch); err != nil {
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
			data, err := c.input(r, *op.Launch)
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
			if err = c.beforeDeadline(*op.Launch); err != nil {
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
		op.Stage = stageComplete
		r.PendingOperation = ""
		return r, c.save(r)
	}
	return r, nil
}
func checkObservation(l Launch, o Observation) error {
	if !SameLaunch(l, o.Launch) {
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
