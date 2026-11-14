package work

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/deadline"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/workspace"
	"github.com/google/uuid"
)

// uploadSuffix names an uploaded workspace the catalogue keeps until the
// coordinator retains it as the first visit's input.
const uploadSuffix = ".workspace"

// exitWatchInterval is how often the exit sweep looks for interactive
// sessions whose harness ended on its own. It is slower than home's five
// seconds: each check of an active session reads its registration, Run and
// Pod from the Kubernetes API.
const exitWatchInterval = 15 * time.Second

// exitCheckTimeout bounds one session's exit check, so one slow placement
// call cannot hold up the sweep.
const exitCheckTimeout = 30 * time.Second

// problemTurnPending explains a stop still waiting for the current turn.
const problemTurnPending = "the stop waits for the agent's current turn to finish; run stop again with the same operation, or cancel"

// interactive reads id's version 2 record of an interactive session the
// caller may act on. A task, or a version 1 record, takes no interactive
// owner action.
func (o *Owner) interactive(id string) (record, error) {
	e, err := o.entry(id)
	if err != nil {
		return record{}, err
	}
	if err = o.access(e.association()); err != nil {
		return record{}, err
	}
	if e.legacy != nil || e.current.Intent.Mode != ModeInteractive {
		return record{}, &profile.Rejection{Code: profile.CodeUnsupported}
	}
	return *e.current, nil
}

// Stop stops an interactive session's current execution gracefully, after
// its current turn, and keeps its checkpoint. The operation is bound to that
// execution, so a replay repeats the same stop. A stop still waiting for the
// turn returns the session as stopping, with a problem that says so.
func (o *Owner) Stop(ctx context.Context, id, operation string) (View, error) {
	if !CanonicalID(operation) {
		return View{}, &profile.Rejection{Code: profile.CodeInvalid}
	}
	if _, err := o.interactive(id); err != nil {
		return View{}, err
	}
	return o.stop(ctx, id, operation)
}

// stop binds operation to the session's current execution and finalizes it
// through the coordinator.
func (o *Owner) stop(ctx context.Context, id, operation string) (View, error) {
	unlock := o.jobs.Lock(id)
	r, err := o.read(id)
	if err != nil {
		unlock()
		return View{}, err
	}
	execution, err := o.bindStop(&r, operation)
	unlock()
	if err != nil {
		return View{}, err
	}
	_, err = o.config.Coordinator.Finalize(ctx, execution, operation)
	switch {
	case errors.Is(err, harness.ErrNativeTurnPending):
		o.setProblem(id, problemTurnPending)
		return o.Info(ctx, id)
	case errors.Is(err, session.ErrSessionCancelled):
		return View{}, &profile.Rejection{Code: profile.CodeConflict}
	case err != nil:
		o.logf("Could not stop work session %s: %v", id, err)
		return View{}, err
	}
	o.setProblem(id, "")
	return o.Info(ctx, id)
}

// bindStop binds a stop operation to the session's current execution and
// saves the record; the caller holds the session's lock. A replay names the
// execution the operation was bound to.
func (o *Owner) bindStop(r *record, operation string) (string, error) {
	if r.Transfer != "" || operation == r.Intent.RequestID {
		return "", &profile.Rejection{Code: profile.CodeConflict}
	}
	if bound, ok := r.Operations[operation]; ok {
		if bound.Action != actionStop {
			return "", &profile.Rejection{Code: profile.CodeConflict}
		}
		return bound.Execution, nil
	}
	rec, err := o.config.Coordinator.Inspect(r.Current)
	if os.IsNotExist(err) || err == nil && rec.Phase.Starting() {
		// Nothing runs yet: cancel the session instead.
		return "", &profile.Rejection{Code: profile.CodeConflict}
	} else if err != nil {
		return "", err
	}
	if len(r.Operations) >= maxOperations {
		return "", &profile.Rejection{Code: profile.CodeUnavailable}
	}
	if r.Operations == nil {
		r.Operations = map[string]boundOperation{}
	}
	r.Operations[operation] = boundOperation{Action: actionStop, Execution: r.Current}
	return r.Current, o.save(*r)
}

// setProblem records the owner's latest problem with the session, or clears it.
func (o *Owner) setProblem(id, problem string) {
	unlock := o.jobs.Lock(id)
	defer unlock()
	if r, err := o.read(id); err == nil && r.Problem != problem {
		r.Problem = problem
		if err = o.save(r); err != nil {
			o.logf("Could not record work session %s: %v", id, err)
		}
	}
}

// Resume starts a new execution of a stopped interactive session, from its
// last checkpoint, or from the workspace it started from when it stopped
// with no conversation. The execution has its own deadline, from
// maxDuration (empty takes the submission's choice) within the executor's
// cap, and the full allowance. The operation is bound to that execution: a
// replay, also after a restart, returns it and never starts another. A
// session that is running, cancelled, being pulled or pulled is refused.
func (o *Owner) Resume(ctx context.Context, id, operation, maxDuration string) (View, error) {
	if !CanonicalID(operation) {
		return View{}, &profile.Rejection{Code: profile.CodeInvalid}
	}
	if _, err := o.interactive(id); err != nil {
		return View{}, err
	}
	unlock := o.jobs.Lock(id)
	defer unlock()
	r, err := o.read(id)
	if err != nil {
		return View{}, err
	}
	if r.Transfer != "" || operation == r.Intent.RequestID {
		return View{}, &profile.Rejection{Code: profile.CodeConflict}
	}
	if bound, ok := r.Operations[operation]; ok {
		if bound.Action != actionResume || bound.Execution != r.Current {
			return View{}, &profile.Rejection{Code: profile.CodeConflict}
		}
		o.dispatch(id)
		return o.entryView(entry{current: &r})
	}
	rec, err := o.execution(r)
	if err != nil {
		return View{}, err
	}
	if rec == nil || rec.Phase != session.PhaseIdle || rec.PendingOperation != "" || rec.CancelOperation != "" {
		return View{}, &profile.Rejection{Code: profile.CodeConflict}
	}
	if len(r.Executions) >= maxExecutions || len(r.Operations) >= maxOperations {
		return View{}, &profile.Rejection{Code: profile.CodeUnavailable}
	}
	execution, err := o.resumedExecution(r, *rec, operation, cmp.Or(maxDuration, r.DeadlineChoice))
	if err != nil {
		return View{}, err
	}
	r.Executions = append(r.Executions, execution.ID)
	r.Current, r.Problem = execution.ID, ""
	if r.Operations == nil {
		r.Operations = map[string]boundOperation{}
	}
	r.Operations[operation] = boundOperation{Action: actionResume, Execution: execution.ID}
	if err = o.save(r); err != nil {
		return View{}, err
	}
	o.dispatch(id)
	return o.view(r, &execution), nil
}

// resumedExecution gives the coordinator the new execution of resume
// operation. Its ID is derived from the session and operation, so a resume
// interrupted after Create adopts the execution it created.
func (o *Owner) resumedExecution(r record, last session.Record, operation, choice string) (session.Record, error) {
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(r.ID+"/resume/"+operation)).String()
	if existing, err := o.config.Coordinator.Inspect(id); err == nil {
		if existing.Session != r.ID || existing.Operations[0].ID != operation {
			return session.Record{}, &profile.Rejection{Code: profile.CodeConflict}
		}
		return existing, nil
	} else if !os.IsNotExist(err) {
		return session.Record{}, err
	}
	_, until, err := o.taskDeadline(CallerHome, choice, time.Now())
	if err != nil {
		return session.Record{}, err
	}
	allowances, err := o.allowances(r.Association)
	if err != nil {
		return session.Record{}, err
	}
	first := session.Launch{Harness: r.First.Harness, ExecutionID: id, OperationID: operation,
		AllocationID: uuid.NewString(), Epoch: 1, Placement: o.config.Placement,
		InstructionDigest: o.config.InstructionDigest, Login: r.First.Login, Deadline: until.Time().UTC(),
		Allowances: allowances, ProfileDigest: r.First.ProfileDigest}
	var input []byte
	if last.Checkpoint.Path != "" {
		if input, err = o.config.Coordinator.Checkpoint(last.ID); err != nil {
			return session.Record{}, err
		}
		if err = checkpoint.ValidateContinuity(input, first.Harness, first.InstructionDigest, o.config.MaxCheckpointBytes); err != nil {
			return session.Record{}, &profile.Rejection{Code: profile.CodeUnsupported}
		}
		first.ThreadID, first.CheckpointDigest = last.ThreadID, last.Checkpoint.Metadata.SHA256
	} else if input, err = o.config.Coordinator.Workspace(last.ID); err != nil {
		// Stopped with no conversation: start one again from its workspace.
		return session.Record{}, err
	}
	return o.config.Coordinator.Create(first, input, r.ID)
}

// allowances is a new execution's full allowance: the executor's, narrowed
// to the session's project, which must still admit this installation.
func (o *Owner) allowances(a project.Association) (map[string]session.Allowance, error) {
	if !project.Member(o.config.Projects, a.Project, o.config.Principal) {
		return nil, &profile.Rejection{Code: profile.CodeDenied}
	}
	if a.Project == "" {
		return maps.Clone(o.config.Allowances), nil
	}
	return project.Narrow(o.config.Projects[a.Project].Capabilities, o.config.Allowances)
}

// recoverInteractive retries an interactive session's unsettled step now,
// once: a stop or cancellation the coordinator could not settle, or a start
// that failed. The executor never retries these by itself.
func (o *Owner) recoverInteractive(ctx context.Context, r record, v View) (View, error) {
	if r.Transfer != "" || v.Phase.Settled() || v.Phase == phaseActive {
		return v, nil
	}
	rec, err := o.execution(r)
	if err != nil {
		return View{}, err
	}
	if rec == nil || rec.CancelOperation == "" && rec.Phase.Starting() {
		o.wake(r.ID)
		return v, nil
	}
	if _, err = o.config.Coordinator.Recover(ctx, rec.ID); errors.Is(err, harness.ErrNativeTurnPending) {
		o.setProblem(r.ID, problemTurnPending)
	} else if err != nil {
		return View{}, err
	} else {
		o.setProblem(r.ID, "")
	}
	return o.Info(ctx, r.ID)
}

// Attachment carries a native terminal's frames to an interactive session's
// active visit.
type Attachment struct {
	terminal    Terminal
	launch      session.Launch
	incarnation session.Incarnation
}

// Run attaches input and output, framed as the placement's terminal
// protocol, until either ends or the execution's deadline passes. Ending an
// attachment never stops the session.
func (a *Attachment) Run(ctx context.Context, input io.Reader, output io.Writer) error {
	ctx, cancel := deadline.At(a.launch.Deadline).Context(ctx)
	defer cancel()
	return a.terminal.Attach(ctx, a.launch, a.incarnation, input, output)
}

// Attach admits an attachment to an interactive session's active visit,
// which the executor finds in its own journal: the caller names the
// session, never a launch or incarnation, so a first visit that has no
// thread yet can be attached to. A session whose harness has exited is
// stopped, as the exit sweep would, and refused.
func (o *Owner) Attach(ctx context.Context, id string) (*Attachment, error) {
	r, err := o.interactive(id)
	if err != nil {
		return nil, err
	}
	if r.Transfer != "" {
		return nil, &profile.Rejection{Code: profile.CodeConflict}
	}
	if stopped, err := o.settleExit(ctx, id); err != nil {
		return nil, err
	} else if stopped {
		return nil, &profile.Rejection{Code: profile.CodeConflict}
	}
	rec, err := o.execution(r)
	if err != nil {
		return nil, err
	}
	if rec == nil || rec.Phase != session.PhaseActive || rec.Active == nil {
		return nil, &profile.Rejection{Code: profile.CodeConflict}
	}
	return &Attachment{terminal: o.config.Terminal, launch: rec.Active.Launch, incarnation: rec.Active.Incarnation}, nil
}

// watchExits stops, through the ordinary stop, every active interactive
// session whose harness exited on its own, so its status shows the exit
// instead of staying active, and its usage is finalized. Tasks need no
// sweep: their driver stops them.
func (o *Owner) watchExits(ctx context.Context) {
	ticker := time.NewTicker(cmp.Or(o.config.exitInterval, exitWatchInterval))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		ids, err := o.ids()
		if err != nil {
			continue
		}
		for _, id := range ids {
			if ctx.Err() != nil {
				return
			}
			if _, err = o.settleExit(ctx, id); err != nil && ctx.Err() == nil {
				o.logf("Could not stop exited work session %s: %v", id, err)
			}
		}
	}
}

// settleExit stops an interactive session whose current active visit's
// harness exited on its own. It reports whether it did.
func (o *Owner) settleExit(ctx context.Context, id string) (bool, error) {
	r, err := o.read(id)
	if err != nil || r.Intent.Mode != ModeInteractive || r.Transfer != "" {
		return false, nil
	}
	rec, err := o.execution(r)
	if err != nil || rec == nil || rec.Phase != session.PhaseActive || rec.Active == nil || o.config.Terminal == nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, exitCheckTimeout)
	defer cancel()
	exited, err := o.config.Terminal.Exited(ctx, rec.Active.Launch, rec.Active.Incarnation)
	if err != nil || !exited {
		return false, err
	}
	_, err = o.stop(ctx, id, derivedOperation(rec.ID, "exited"))
	return err == nil, err
}

// validateUpload checks an uploaded workspace as the placement checks a
// moved one before anything is kept: the archive's entries and manifest,
// and then a complete restore in private scratch space, which verifies
// every Git object, the index and each file and installs no configuration,
// hooks or credentials. Symbolic links, special files, oversize content
// and malformed archives are refused.
func (o *Owner) validateUpload(ctx context.Context, data []byte) error {
	limit := o.config.MaxCheckpointBytes
	if _, err := workspace.Inspect(data, limit); err != nil {
		return &profile.Rejection{Code: profile.CodeInvalid}
	}
	dir, err := privatefs.TempDir(o.config.Directory, ".sproozi-upload-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err = os.Chmod(dir, 0o700); err != nil {
		return err
	}
	// The archive came from a client: Restore bounds its bytes, and this
	// deadline bounds Git's time on it.
	ctx, cancel := context.WithTimeout(ctx, revisionTimeout)
	defer cancel()
	if err = workspace.Restore(ctx, data, filepath.Join(dir, "workspace"), limit); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		o.logf("Refused an uploaded workspace: %v", err)
		return &profile.Rejection{Code: profile.CodeInvalid}
	}
	return nil
}

// keptUpload reads the uploaded workspace a session starts from.
func (o *Owner) keptUpload(r record) ([]byte, error) {
	data, err := privatefs.ReadFile(o.path(r.ID, uploadSuffix), o.config.MaxCheckpointBytes)
	if os.IsNotExist(err) {
		return nil, errors.New("work: the uploaded workspace is no longer kept; cancel this session and submit again")
	}
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if "sha256:"+hex.EncodeToString(sum[:]) != r.Intent.WorkspaceDigest {
		return nil, errors.New("work: the kept upload changed")
	}
	return data, nil
}

func (o *Owner) removeUpload(id string) {
	if err := os.Remove(o.path(id, uploadSuffix)); err != nil && !os.IsNotExist(err) {
		o.logf("Could not remove the upload of work session %s: %v", id, err)
	}
}

// removeKeptUploads removes uploads nothing needs any more: those of a
// session the coordinator received or that is settled, and those of a
// submission that was never recorded.
func (o *Owner) removeKeptUploads() {
	entries, err := os.ReadDir(o.config.Directory)
	if err != nil {
		return
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), uploadSuffix)
		if !ok || !CanonicalID(id) {
			continue
		}
		r, err := o.read(id)
		if err == nil && r.Transfer == "" && r.CancelOperation == "" {
			if _, err = o.config.Coordinator.Inspect(r.Current); os.IsNotExist(err) {
				continue
			}
		} else if err != nil && !os.IsNotExist(err) {
			continue
		}
		o.removeUpload(id)
	}
}
