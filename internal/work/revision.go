package work

import (
	"errors"
	"os"

	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/project"
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
	if r.Stopped == nil || r.Phase != phaseRetained && r.Phase != phaseHomeOwned && r.Phase != phaseExporting {
		return nil, errors.New("work: a completed retained source workspace is required")
	}
	association := view(r).Association
	if association.Repository != target.Repository || association.RepositoryReference != target.RepositoryReference {
		return nil, errors.New("work: source and target repository identities differ")
	}
	if source.Digest == "" || source.Digest != r.Stopped.CheckpointDigest {
		return nil, errors.New("work: select the exact recorded source checkpoint digest")
	}
	p := o.path(source.SessionID, ".checkpoint")
	if err = private(p, false); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
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
		return nil, errors.New("work: select the exact full recorded workspace revision")
	}
	return data, nil
}
