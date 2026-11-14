package work

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"

	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
)

// exportedStop describes a stopped visit as an exported continuation, whose
// launch and receipt name the native thread to resume. The placement reports a
// first visit's genuine thread separately because its launch never changes.
func exportedStop(s session.Stopped) session.Stopped {
	thread := cmp.Or(s.ThreadID, s.Launch.ThreadID)
	s.Launch.ThreadID, s.ThreadID = thread, ""
	if s.Receipt != nil {
		receipt := *s.Receipt
		receipt.Launch.ThreadID = thread
		s.Receipt = &receipt
	}
	return s
}

// ValidateExport checks an export's writer stop, sealed usage and checkpoint
// evidence, keeping transcript validation separate from that evidence. The
// stopped launch can belong to a later execution of the exported session, so
// callers bind the execution themselves.
func ValidateExport(e Export, max int64) error {
	s := e.Stopped
	if !CanonicalID(e.ID) || e.Harness != s.Launch.Harness || !s.ActualStopped || s.Receipt == nil || s.Receipt.Incarnation != s.Incarnation || s.Receipt.Launch.ThreadID != s.Launch.ThreadID || len(s.Receipt.Usage) != len(s.Launch.Allowances) {
		return &profile.Rejection{Code: profile.CodeUnavailable}
	}
	for cap, a := range s.Launch.Allowances {
		u, ok := s.Receipt.Usage[cap]
		if !ok || u.Units < 0 || u.CostMicros < 0 || a.Units.Bounded && u.Units > a.Units.Value || a.CostMicros.Bounded && u.CostMicros > a.CostMicros.Value {
			return &profile.Rejection{Code: profile.CodeUnavailable}
		}
	}
	a, _ := json.Marshal(s.Launch)
	b, _ := json.Marshal(s.Receipt.Launch)
	if string(a) != string(b) {
		return &profile.Rejection{Code: profile.CodeUnavailable}
	}
	meta, err := checkpoint.Inspect(e.Checkpoint, max)
	if err != nil {
		return &profile.Rejection{Code: profile.CodeUnsupported}
	}
	if meta.Harness != e.Harness || meta.ThreadID != s.Launch.ThreadID || meta.SHA256 != s.CheckpointDigest {
		return &profile.Rejection{Code: profile.CodeUnsupported}
	}
	if e.Project != "" && !project.ValidID(e.Project) {
		return &profile.Rejection{Code: profile.CodeUnsupported}
	}
	if project.ValidateReference(e.RepositoryReference) != nil {
		return &profile.Rejection{Code: profile.CodeUnsupported}
	}
	if e.CheckpointInfo != nil {
		a, _ := json.Marshal(e.CheckpointInfo)
		b, _ := json.Marshal(meta)
		if !bytes.Equal(a, b) {
			return &profile.Rejection{Code: profile.CodeUnsupported}
		}
	}
	if checkpoint.ValidateContinuity(e.Checkpoint, e.Harness, s.Launch.InstructionDigest, max) != nil {
		return &profile.Rejection{Code: profile.CodeUnsupported}
	}
	return nil
}

// stoppedVisit is the coordinator's evidence for a stopped execution, as an
// export describes it: the last visit's launch, incarnation and sealed
// receipt, the execution's thread, and its committed checkpoint. The
// execution must be idle, with nothing pending, not cancelled, and a
// checkpoint committed.
func (o *Owner) stoppedVisit(rec *session.Record) (session.Stopped, []byte, error) {
	if rec == nil || rec.Phase != session.PhaseIdle || rec.PendingOperation != "" || rec.CancelOperation != "" ||
		rec.Checkpoint.Path == "" || len(rec.Visits) == 0 {
		return session.Stopped{}, nil, &profile.Rejection{Code: profile.CodeConflict}
	}
	last := rec.Visits[len(rec.Visits)-1]
	data, err := o.config.Coordinator.Checkpoint(rec.ID)
	if err != nil {
		return session.Stopped{}, nil, err
	}
	return exportedStop(session.Stopped{Launch: last.Launch, Incarnation: last.Incarnation,
		ActualStopped: last.ActualStopped, ThreadID: rec.ThreadID, Receipt: last.Receipt,
		CheckpointDigest: rec.Checkpoint.Metadata.SHA256}), data, nil
}

func (o *Owner) BeginExport(ctx context.Context, id, operation string) (Export, error) {
	return o.export(ctx, id, operation, "", false)
}
func (o *Owner) Claim(ctx context.Context, id, operation, digest string) (Export, error) {
	return o.export(ctx, id, operation, digest, true)
}

// export hands a stopped session's evidence to the home principal. The
// first call binds the pull operation; a claim with that operation and the
// checkpoint's digest transfers ownership, once.
func (o *Owner) export(ctx context.Context, id, operation, digest string, claim bool) (Export, error) {
	if !CanonicalID(id) || !CanonicalID(operation) {
		return Export{}, &profile.Rejection{Code: profile.CodeInvalid}
	}
	if ctx.Err() != nil {
		return Export{}, ctx.Err()
	}
	e, err := o.entry(id)
	if err != nil {
		return Export{}, err
	}
	if err = o.access(e.association()); err != nil {
		return Export{}, err
	}
	unlock := o.jobs.Lock(id)
	defer unlock()
	if e.legacy != nil {
		return o.exportLegacy(id, operation, digest, claim)
	}
	r, err := o.read(id)
	if err != nil {
		return Export{}, err
	}
	rec, err := o.execution(r)
	if err != nil {
		return Export{}, err
	}
	if !derivePhase(r, rec).stopped() || r.Transfer != "" && r.Transfer != operation {
		return Export{}, &profile.Rejection{Code: profile.CodeConflict}
	}
	if claim && (r.Transfer != operation || digest != rec.Checkpoint.Metadata.SHA256) {
		return Export{}, &profile.Rejection{Code: profile.CodeConflict}
	}
	stopped, data, err := o.stoppedVisit(rec)
	if err != nil {
		return Export{}, err
	}
	info, err := checkpoint.Inspect(data, o.config.MaxCheckpointBytes)
	if err != nil {
		return Export{}, err
	}
	out := Export{Association: o.view(r, rec).Association, CheckpointInfo: &info, ID: id, Harness: r.First.Harness,
		Stopped: stopped, Checkpoint: data, OperationID: operation, Claimed: r.Pulled}
	if err = ValidateExport(out, o.config.MaxCheckpointBytes); err != nil {
		o.logf("Could not export work session %s: %v", id, err)
		return Export{}, err
	}
	r.Transfer = operation
	if claim {
		r.Pulled, out.Claimed = true, true
	}
	if err = o.save(r); err != nil {
		return Export{}, err
	}
	return out, nil
}
