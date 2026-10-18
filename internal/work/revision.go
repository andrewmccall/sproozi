package work

import (
	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session/profile"
)

// RevisionSource binds a fresh conversation to an immutable stopped workspace,
// independently of its original native history and grant.
type RevisionSource struct {
	SessionID string `json:"sessionId"`
	Digest    string `json:"digest"`
	Revision  string `json:"revision"`
}

func (o *Owner) revisionSource(source RevisionSource, target project.Association) ([]byte, error) {
	r, err := o.read(source.SessionID)
	if err != nil {
		return nil, err
	}
	if err = o.access(r); err != nil {
		return nil, err
	}
	if r.Stopped == nil || !r.Phase.stopped() {
		return nil, &profile.Rejection{Code: profile.CodeConflict}
	}
	association := view(r).Association
	if association.Repository != target.Repository || association.RepositoryReference != target.RepositoryReference {
		return nil, &profile.Rejection{Code: profile.CodeDenied}
	}
	if source.Digest == "" || source.Digest != r.Stopped.CheckpointDigest {
		return nil, &profile.Rejection{Code: profile.CodeInvalid}
	}
	p := o.path(source.SessionID, ".checkpoint")
	data, err := privatefs.ReadFile(p, o.config.MaxCheckpointBytes)
	if err != nil {
		return nil, err
	}
	if err = ValidateExport(Export{ID: source.SessionID, Harness: r.Launch.Harness, Stopped: *r.Stopped,
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
