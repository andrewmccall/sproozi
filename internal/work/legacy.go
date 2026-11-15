package work

// The read-only version 1 reader. Version 1 records kept their own execution
// lifecycle; ADR 0057 moved execution to the coordinator. A stopped version 1
// record still lists, exports and claims from its own record and kept
// checkpoint, so retained work can be pulled after the upgrade. A live one
// stops the executor from starting (Open). Remove this file in a later minor
// release.

import (
	"cmp"
	"encoding/json"
	"errors"

	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
)

const legacyVersion = 1

// legacyPhase is a version 1 record's stored phase.
type legacyPhase string

const (
	legacyRetained  legacyPhase = "retained"
	legacyExporting legacyPhase = "exporting"
	legacyPulled    legacyPhase = "pulled"
	legacyCancelled legacyPhase = "cancelled"
)

// stoppedOrCancelled reports a version 1 phase nothing executes any more.
func (p legacyPhase) stoppedOrCancelled() bool {
	return p == legacyRetained || p == legacyExporting || p == legacyPulled || p == legacyCancelled
}

// legacyRecord is the version 1 record.
type legacyRecord struct {
	Version          int                  `json:"version"`
	Intent           Submit               `json:"intent"`
	IntentDigest     string               `json:"intentDigest"`
	Launch           session.Launch       `json:"launch"`
	Phase            legacyPhase          `json:"phase"`
	Progress         legacyPhase          `json:"progress,omitempty"`
	Incarnation      *session.Incarnation `json:"incarnation,omitempty"`
	Stopped          *session.Stopped     `json:"stopped,omitempty"`
	InputDigest      string               `json:"inputDigest,omitempty"`
	Result           string               `json:"result,omitempty"`
	ResultTruncated  bool                 `json:"resultTruncated,omitempty"`
	Problem          string               `json:"problem,omitempty"`
	Transfer         string               `json:"transfer,omitempty"`
	CancelOperation  string               `json:"cancelOperation,omitempty"`
	Association      project.Association  `json:"association,omitempty"`
	RepositorySource *Repository          `json:"repositorySource,omitempty"`
	CheckpointInfo   *checkpoint.Metadata `json:"checkpointInfo,omitempty"`
}

func (o *Owner) readLegacy(id string, b []byte) (legacyRecord, error) {
	var r legacyRecord
	if err := decode(b, &r); err != nil {
		return r, err
	}
	intent, _ := json.Marshal(r.Intent)
	if r.Version != legacyVersion || o.sessionID(r.Intent.RequestID) != id || r.Launch.ExecutionID != id || r.IntentDigest != hash(intent) || r.Launch.Harness != o.config.Harness || r.Launch.Placement != o.config.Placement || r.Launch.InstructionDigest != o.config.InstructionDigest || !CanonicalID(r.Launch.OperationID) || !CanonicalID(r.Launch.AllocationID) {
		return r, errors.New("work: retained owner binding changed")
	}
	if err := o.checkAssociation(r.Association, r.Intent.Project); err != nil {
		return r, err
	}
	return r, nil
}

func (o *Owner) saveLegacy(r legacyRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return privatefs.Replace(o.config.Directory, r.Launch.ExecutionID+".json", b)
}

func legacyView(r legacyRecord) View {
	association := r.Association
	if association.Repository == "" {
		association.Repository = r.Intent.Repository
	}
	v := View{Association: association, ID: r.Launch.ExecutionID, ExecutionID: r.Launch.ExecutionID,
		AllocationID: r.Launch.AllocationID, Placement: r.Launch.Placement, Incarnation: r.Incarnation,
		Harness: r.Launch.Harness, Phase: Phase(r.Phase), Deadline: r.Launch.Deadline, Login: r.Launch.Login,
		Result: r.Result, ResultTruncated: r.ResultTruncated, Problem: r.Problem}
	v.CheckpointInfo, v.WorkspaceFrom = r.CheckpointInfo, r.Intent.WorkspaceFrom
	if r.Stopped != nil {
		v.ThreadID = cmp.Or(r.Stopped.ThreadID, r.Stopped.Launch.ThreadID)
		v.CheckpointDigest = r.Stopped.CheckpointDigest
	}
	return v
}

// legacyStopped is a stopped version 1 record's evidence and kept checkpoint.
func (o *Owner) legacyStopped(r legacyRecord) (session.Stopped, []byte, error) {
	if !Phase(r.Phase).stopped() || r.Stopped == nil || r.CancelOperation != "" {
		return session.Stopped{}, nil, &profile.Rejection{Code: profile.CodeConflict}
	}
	data, err := privatefs.ReadFile(o.path(r.Launch.ExecutionID, ".checkpoint"), o.config.MaxCheckpointBytes)
	return *r.Stopped, data, err
}

// exportLegacy is export for a version 1 record. The caller holds the lock.
func (o *Owner) exportLegacy(id, operation, digest string, claim bool) (Export, error) {
	b, err := privatefs.ReadFile(o.path(id, ".json"), maxRecordBytes)
	if err != nil {
		return Export{}, err
	}
	r, err := o.readLegacy(id, b)
	if err != nil {
		return Export{}, err
	}
	stopped, data, err := o.legacyStopped(r)
	if err != nil {
		return Export{}, err
	}
	if r.Transfer != "" && r.Transfer != operation {
		return Export{}, &profile.Rejection{Code: profile.CodeConflict}
	}
	if claim && (r.Transfer != operation || digest != stopped.CheckpointDigest) {
		return Export{}, &profile.Rejection{Code: profile.CodeConflict}
	}
	info, err := checkpoint.Inspect(data, o.config.MaxCheckpointBytes)
	if err != nil {
		return Export{}, err
	}
	e := Export{Association: legacyView(r).Association, CheckpointInfo: &info, ID: id, Harness: r.Launch.Harness,
		Stopped: stopped, Checkpoint: data, OperationID: operation, Claimed: r.Phase == legacyPulled}
	if err = ValidateExport(e, o.config.MaxCheckpointBytes); err != nil {
		o.logf("Could not export work session %s: %v", id, err)
		return Export{}, err
	}
	r.Transfer = operation
	if claim {
		r.Phase = legacyPulled
		e.Claimed = true
	} else if r.Phase != legacyPulled {
		r.Phase = legacyExporting
	}
	if err = o.saveLegacy(r); err != nil {
		return Export{}, err
	}
	return e, nil
}

// cancelLegacy answers a cancellation of a version 1 record, which is always
// stopped or cancelled: a retried cancellation of a cancelled one reports it.
func (o *Owner) cancelLegacy(r legacyRecord, operation string) (View, error) {
	if r.Phase == legacyCancelled && r.CancelOperation == operation {
		return legacyView(r), nil
	}
	return View{}, &profile.Rejection{Code: profile.CodeConflict}
}
