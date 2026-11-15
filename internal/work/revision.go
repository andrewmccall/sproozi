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
	return data, nil
}
