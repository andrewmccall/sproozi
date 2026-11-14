package work

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/deadline"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/workspace"
)

// Problems the driver records for a session it ended itself.
const (
	problemExpired = "the deadline passed before it started"
	problemRevoked = "its project no longer admits this installation"
)

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
// through the coordinator. wake asks it to retry now.
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

// step takes the session's next step through the coordinator.
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
	coordinator := o.config.Coordinator
	revoked := !project.Member(o.config.Projects, r.Association.Project, o.config.Principal)
	switch {
	case rec != nil && rec.Phase == session.PhaseCancelled:
		return nextDone
	case rec != nil && rec.Phase == session.PhaseIdle:
		return o.finish(r)
	case rec != nil && rec.CancelOperation != "":
		// Recovery carries out the retained cancellation.
		_, err = coordinator.Recover(ctx, rec.ID)
		return o.after(ctx, r, err, nextWait)
	case revoked:
		return o.end(ctx, r, "project-revoked", problemRevoked)
	case rec == nil:
		return o.start(ctx, r)
	case rec.Phase.Starting():
		if deadline.At(rec.Deadline).Passed(time.Now()) {
			return o.end(ctx, r, "deadline-expired", problemExpired)
		}
		_, err = coordinator.Recover(ctx, rec.ID)
		return o.after(ctx, r, err, nextDone)
	case rec.Phase == session.PhaseActive || rec.Phase == session.PhaseStopping:
		// The task's stop is its completion wait: the supervisor stops the
		// writer only after the task's own turn completed. Until then the
		// stop stays pending and is repeated.
		_, err = coordinator.Finalize(ctx, rec.ID, derivedOperation(rec.ID, "task-complete"))
		if errors.Is(err, harness.ErrNativeTurnPending) {
			o.note(r.ID, "the executor waits for the task's turn to complete: "+err.Error())
			return nextAgain
		}
		return o.after(ctx, r, err, nextWait)
	default:
		// An unproven stop or unsealed usage: recovery never makes up a
		// stop or a refund, so retrying it is safe.
		_, err = coordinator.Recover(ctx, rec.ID)
		return o.after(ctx, r, err, nextWait)
	}
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
// or its deadline passed before it started. A session that already stopped
// keeps its result.
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
// coordinator.
func (o *Owner) start(ctx context.Context, r record) next {
	if deadline.At(r.First.Deadline).Passed(time.Now()) {
		return o.end(ctx, r, "deadline-expired", problemExpired)
	}
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
	if _, err = o.config.Coordinator.Create(r.First, input, ""); err != nil {
		if deadline.At(r.First.Deadline).Passed(time.Now()) {
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
