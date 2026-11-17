package work

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/workspace"
)

// Problems the driver records for a session it ended itself.
const (
	problemExpired = "the deadline passed before it started"
	problemRevoked = "its project no longer admits this installation"
	// problemUndecided: the driver has no rule for the session's state.
	problemUndecided = "the executor has no rule for this session's state; it keeps retrying, and the executor's log names the state"
)

// actionTaskComplete derives the operation of a task's completion wait: the
// stop the driver asks for at once, which the supervisor carries out only
// after the task's own turn completed.
const actionTaskComplete = "task-complete"

// backoff bounds the driver's automatic recovery of an execution the
// coordinator could not settle.
type backoff struct{ first, limit time.Duration }

func (b backoff) orDefault() backoff {
	if b.first <= 0 {
		b.first = time.Second
	}
	if b.limit < b.first {
		b.limit = max(time.Minute, b.first)
	}
	return b
}

// driver is the one background goroutine that carries a session's task
// through the coordinator, or starts an interactive session's execution.
// wake asks it to retry now.
type driver struct {
	stop context.CancelFunc
	wake chan struct{}
}

// dispatch starts the session's driver unless one runs.
func (o *Owner) dispatch(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ctx.Err() != nil || o.running[id] != nil {
		return
	}
	ctx, cancel := context.WithCancel(o.ctx)
	d := &driver{stop: cancel, wake: make(chan struct{}, 1)}
	o.running[id] = d
	o.wg.Go(func() {
		defer cancel()
		for {
			o.drive(ctx, id, d.wake)
			o.mu.Lock()
			select {
			case <-d.wake:
				// A recover arrived as the driver finished: drive again.
				o.mu.Unlock()
				if ctx.Err() == nil {
					continue
				}
				o.mu.Lock()
			default:
			}
			delete(o.running, id)
			o.mu.Unlock()
			return
		}
	})
}

// wake makes the session's driver retry now, starting one if none runs.
func (o *Owner) wake(id string) {
	o.mu.Lock()
	d := o.running[id]
	if d != nil {
		select {
		case d.wake <- struct{}{}:
		default:
		}
	}
	o.mu.Unlock()
	if d == nil {
		o.dispatch(id)
	}
}

// next is what the driver does after a step.
type next int

const (
	// nextDone: the session needs nothing more from the driver.
	nextDone next = iota
	// nextAgain: take the next step now.
	nextAgain
	// nextWait: retry after the backoff, or sooner when woken.
	nextWait
)

// drive carries the session's task until it settles, or until a step fails
// in a way only a recover retries.
func (o *Owner) drive(ctx context.Context, id string, wake <-chan struct{}) {
	retry := o.config.retry.orDefault()
	wait := retry.first
	for ctx.Err() == nil {
		switch o.step(ctx, id) {
		case nextDone:
			return
		case nextAgain:
			wait = retry.first
		case nextWait:
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
			case <-wake:
			case <-timer.C:
			}
			timer.Stop()
			wait = min(2*wait, retry.limit)
		}
	}
}

// step takes the session's next step through the coordinator: the one
// action decisions maps its situation to. A session whose catalogue records
// a transfer, or a cancellation before the coordinator received it, is no
// longer the driver's, and is left before its execution is read.
func (o *Owner) step(ctx context.Context, id string) next {
	r, err := o.read(id)
	if err != nil {
		o.logf("Could not read work session %s: %v", id, err)
		return nextDone
	}
	if r.Transfer != "" || r.CancelOperation != "" {
		return nextDone
	}
	rec, err := o.execution(r)
	if err != nil {
		o.logf("Could not inspect work session %s: %v", id, err)
		return nextWait
	}
	act, err := decide(o.situationOf(r, rec))
	if err != nil {
		// A situation no row decides is a programming error. Say so where
		// the owner sees it, and keep retrying: an executor that does
		// decide it may be running by the next attempt.
		o.logf("Could not decide the next step of work session %s: %v", id, err)
		o.note(id, problemUndecided)
		return nextWait
	}
	return o.carryOut(ctx, r, rec, act)
}

// carryOut takes action act for the session. rec is the coordinator's
// record of its current execution; decisions maps only a session the
// coordinator has not received to an action that does not read it.
func (o *Owner) carryOut(ctx context.Context, r record, rec *session.Record, act action) next {
	coordinator := o.config.Coordinator
	switch act {
	case actStart:
		return o.start(ctx, r)
	case actRecoverStart:
		// Past its deadline, an interactive session's start ends instead
		// of starting: it stays stopped with what its earlier execution
		// committed, and resume continues it (ADR 0060).
		_, err := coordinator.Recover(ctx, rec.ID)
		if errors.Is(err, session.ErrDeadlinePassed) {
			o.note(r.ID, problemExpired)
			return nextAgain
		}
		return o.after(ctx, r, err, nextDone)
	case actComplete:
		// The task's stop is its completion wait: the supervisor stops the
		// writer only after the task's own turn completed. Until then the
		// stop stays pending and is repeated. The task is running, not
		// stopping (see completing), and nothing is wrong with it, so the
		// wait clears any earlier problem rather than recording one.
		_, err := coordinator.Finalize(ctx, rec.ID, derivedOperation(rec.ID, actionTaskComplete))
		if errors.Is(err, harness.ErrNativeTurnPending) {
			o.note(r.ID, "")
			return nextAgain
		}
		return o.after(ctx, r, err, nextWait)
	case actRecover:
		// Recovery carries out a retained cancellation, or an unproven stop
		// or unsealed usage: it never makes up a stop or a refund, so
		// retrying it is safe. A retried stop can find the writer reachable
		// after all and still waiting for the task's turn: the coordinator
		// then holds it as stopping again, and the next step is the
		// completion wait. A held cancellation is carried out by aborting,
		// which never waits for a turn.
		recovered, err := coordinator.Recover(ctx, rec.ID)
		if errors.Is(err, harness.ErrNativeTurnPending) && recovered.Phase == session.PhaseStopping {
			o.note(r.ID, "")
			return nextAgain
		}
		return o.after(ctx, r, err, nextWait)
	case actFinish:
		return o.finish(r)
	case actEndExpired:
		// A task whose start the deadline ended, even inside a recovery,
		// has no result to finish: nothing ran.
		return o.end(ctx, r, "deadline-expired", problemExpired)
	case actEndRevoked:
		return o.end(ctx, r, "project-revoked", problemRevoked)
	case actNothing:
	}
	return nextDone
}

// startEnded reports an execution whose deadline ended its first visit
// before it started (ADR 0060), so it holds no result. Once that visit is
// ended the execution starts nothing again, so the only visit it can have is
// the prepared incarnation the end aborted: that keeps its sealed usage, and
// a checkpoint only if the abort captured one. A task's result is read from
// its checkpoint, and its first visit starts from a workspace, so with no
// checkpoint there is nothing a cancellation could discard. A cancellation
// of the idle execution changes neither its visits nor its allowance.
func startEnded(rec session.Record) bool {
	return rec.Abandoned(rec.Operations[0].ID) && rec.Checkpoint.Path == ""
}

// after decides what follows a coordinator call. Success takes the next step.
// An interrupted call waits, because a cancellation is in progress. Any
// other failure is recorded; the driver then retries after a backoff
// (onError nextWait) or stops until a recover (nextDone).
func (o *Owner) after(ctx context.Context, r record, err error, onError next) next {
	if err == nil {
		o.note(r.ID, "")
		return nextAgain
	}
	if ctx.Err() != nil {
		return nextDone
	}
	if errors.Is(err, context.Canceled) {
		return nextWait
	}
	o.logf("Could not continue work session %s: %v", r.ID, err)
	if onError == nextDone {
		o.note(r.ID, outcomeMessage(err)+"; inspect status and recover this session")
	}
	return onError
}

// note records the driver's latest problem with the session, or clears it.
func (o *Owner) note(id, problem string) {
	unlock := o.jobs.Lock(id)
	defer unlock()
	r, err := o.read(id)
	if err != nil || r.Problem == problem || r.Finished || r.CancelOperation != "" ||
		r.Problem == problemExpired || r.Problem == problemRevoked {
		return
	}
	r.Problem = problem
	if err = o.save(r); err != nil {
		o.logf("Could not record work session %s: %v", id, err)
	}
}

// end cancels a session the executor must not run: its project was revoked,
// or its deadline passed before it started and it has nothing to keep: a
// task, or a session the coordinator never received. A session that already
// stopped keeps its result.
func (o *Owner) end(ctx context.Context, r record, action, problem string) next {
	err := o.cancelExecution(ctx, r.ID, derivedOperation(r.Current, action), problem)
	if err == nil || OutcomeCode(err) == profile.CodeConflict {
		return nextAgain
	}
	if ctx.Err() == nil {
		o.logf("Could not cancel work session %s: %v", r.ID, err)
	}
	return nextWait
}

// start captures the session's input and gives its first execution to the
// coordinator. decisions ends a session whose deadline passed first.
func (o *Owner) start(ctx context.Context, r record) next {
	input, err := o.capture(ctx, r)
	if err != nil {
		return o.after(ctx, r, err, nextDone)
	}
	unlock := o.jobs.Lock(r.ID)
	defer unlock()
	if r, err = o.read(r.ID); err != nil || r.CancelOperation != "" || r.Transfer != "" {
		return nextAgain
	}
	if _, err = o.config.Coordinator.Inspect(r.Current); err == nil {
		return nextAgain
	}
	if _, err = o.config.Coordinator.Create(r.First, input, ""); err == nil && r.Intent.WorkspaceDigest != "" {
		// The coordinator retains the upload now.
		o.removeUpload(r.ID)
	}
	if err != nil {
		if o.pastFirstDeadline(r) {
			// Create refuses a launch whose deadline passed; the next step
			// cancels it.
			return nextAgain
		}
		o.logf("Could not start work session %s: %v", r.ID, err)
		r.Problem = outcomeMessage(err) + "; inspect status and recover this session"
		if saveErr := o.save(r); saveErr != nil {
			o.logf("Could not record work session %s: %v", r.ID, saveErr)
		}
		return nextDone
	}
	return nextAgain
}

// capture is the first execution's input: a revision of retained work, an
// approved snapshot, or an empty workspace for a task that fetches its
// repository. Nothing is kept until the coordinator retains it.
func (o *Owner) capture(ctx context.Context, r record) ([]byte, error) {
	limit := o.config.MaxCheckpointBytes
	if r.Intent.WorkspaceDigest != "" {
		return o.keptUpload(r)
	}
	if r.Intent.WorkspaceFrom != nil {
		data, err := o.revisionSource(*r.Intent.WorkspaceFrom, r.Association)
		if err != nil {
			return nil, err
		}
		archive, _, err := checkpoint.Workspace(data, limit)
		if err != nil {
			return nil, err
		}
		// The archive came from a sandbox. AtRevision bounds its object and
		// file bytes; this deadline bounds Git's time on it.
		revisionCtx, cancel := context.WithTimeout(ctx, revisionTimeout)
		defer cancel()
		return workspace.AtRevision(revisionCtx, archive, r.Intent.WorkspaceFrom.Revision, o.config.Directory, limit)
	}
	repo := o.config.Repositories[r.Intent.Repository]
	if r.RepositorySource != nil {
		repo = *r.RepositorySource
	}
	if repo.Snapshot != "" {
		return workspace.Capture(ctx, repo.Snapshot, nil, limit)
	}
	return workspace.Empty(ctx, o.config.Directory, limit)
}

// finish writes the task's tail once the coordinator committed its stop:
// the checkpoint's metadata and the bounded, untrusted final answer.
func (o *Owner) finish(r record) next {
	unlock := o.jobs.Lock(r.ID)
	defer unlock()
	r, err := o.read(r.ID)
	if err != nil || r.Finished {
		return nextDone
	}
	r.Finished, r.Problem = true, ""
	data, err := o.config.Coordinator.Checkpoint(r.Current)
	if err == nil {
		var info checkpoint.Metadata
		if info, err = checkpoint.Inspect(data, o.config.MaxCheckpointBytes); err == nil {
			r.CheckpointInfo = &info
			r.Result, r.ResultTruncated, err = checkpoint.FinalText(data, o.config.MaxCheckpointBytes)
		}
	} else if rec, inspectErr := o.config.Coordinator.Inspect(r.Current); inspectErr == nil && rec.Checkpoint.Path == "" {
		// The task stopped with no conversation to keep.
		err, r.Problem = nil, "the task stopped before its native conversation began; nothing was kept"
	}
	if err != nil {
		o.logf("Could not read the result of work session %s: %v", r.ID, err)
		return nextWait
	}
	if err = o.save(r); err != nil {
		o.logf("Could not record work session %s: %v", r.ID, err)
		return nextWait
	}
	return nextDone
}

func (o *Owner) logf(format string, args ...any) {
	if o.config.Log != nil {
		_, _ = fmt.Fprintf(o.config.Log, format+"\n", args...)
	}
}
