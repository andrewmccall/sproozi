package work

import (
	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
)

// RevisionSource binds a fresh conversation to an immutable stopped workspace,
// independently of its original native history and grant.
type RevisionSource struct {
	SessionID string `json:"sessionId"`
	Digest    string `json:"digest"`
	Revision  string `json:"revision"`
}

// revisionSource reads the stopped checkpoint a fresh session starts from: the
// coordinator's committed checkpoint of a stopped session, or a version 1
// record's kept checkpoint.
func (o *Owner) revisionSource(source RevisionSource, target project.Association) ([]byte, error) {
	e, err := o.entry(source.SessionID)
	if err != nil {
		return nil, err
	}
	o.interleaveAt(stepSourceRecord, source.SessionID)
	if err = o.access(e.association()); err != nil {
		return nil, err
	}
	var association project.Association
	var stopped session.Stopped
	var kind harness.NativeKind
	var data []byte
	if e.legacy != nil {
		association, kind = legacyView(*e.legacy).Association, e.legacy.Launch.Harness
		if stopped, data, err = o.legacyStopped(*e.legacy); err != nil {
			return nil, err
		}
	} else {
		r := *e.current
		rec, err := o.execution(r)
		if err != nil {
			return nil, err
		}
		o.interleaveAt(stepSourceExecution, source.SessionID)
		if !derivePhase(r, rec).stopped() {
			return nil, &profile.Rejection{Code: profile.CodeConflict}
		}
		association, kind = o.view(r, rec).Association, r.First.Harness
		if stopped, data, err = o.stoppedVisit(rec); err != nil {
			return nil, err
		}
	}
	if association.Repository != target.Repository || association.RepositoryReference != target.RepositoryReference {
		return nil, &profile.Rejection{Code: profile.CodeDenied}
	}
	if source.Digest == "" || source.Digest != stopped.CheckpointDigest {
		return nil, &profile.Rejection{Code: profile.CodeInvalid}
	}
	if err = ValidateExport(Export{ID: source.SessionID, Harness: kind, Stopped: stopped,
		Checkpoint: data}, o.config.MaxCheckpointBytes); err != nil {
		return nil, err
	}
	meta, err := checkpoint.Inspect(data, o.config.MaxCheckpointBytes)
	if err != nil {
		return nil, err
	}
	if source.Revision == "" || source.Revision != meta.Workspace.Revision {
		return nil, &profile.Rejection{Code: profile.CodeInvalid}
	}
	o.interleaveAt(stepSourceCheckpoint, source.SessionID)
	return data, nil
}

// checkSource checks the session a submission starts from before the
// submission is admitted. It reads the source without the locks Remove holds,
// so a removal can delete the source part way through the read. Remove marks
// the session removed before it deletes anything, and the mark stays, so a
// read that failed while the source went is answered as one that found it
// removed. The check under o.retention answers a removal after the read.
func (o *Owner) checkSource(source RevisionSource, target project.Association) error {
	_, err := o.revisionSource(source, target)
	if err != nil && CanonicalID(source.SessionID) && o.wasRemoved(source.SessionID) {
		return &profile.Rejection{Code: profile.CodeInvalid}
	}
	return err
}

// The steps at which Config.interleave runs, each with the ID of the
// session the step read. revisionSource reads the session new work starts
// from: its catalogue record, the coordinator's record of its execution, and
// its checkpoint, read and checked. stepEntry follows every read of a
// catalogue record by entry; stepStopBound follows the save of a stop's
// binding, before the coordinator finalizes the execution.
const (
	stepSourceRecord     = "record"
	stepSourceExecution  = "execution"
	stepSourceCheckpoint = "checkpoint"
	stepEntry            = "entry"
	stepStopBound        = "stop-bound"
)

// interleaveAt runs the test hook Config.interleave, if set, after step read
// session id.
func (o *Owner) interleaveAt(step, id string) {
	if o.config.interleave != nil {
		o.config.interleave(step, id)
	}
}
