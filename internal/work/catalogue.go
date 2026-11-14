package work

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
)

// recordVersion 2 keeps what the coordinator does not; version 1 records,
// which kept their own execution lifecycle, are read by legacy.go.
const recordVersion = 2

// maxRecordBytes bounds one catalogue record.
const maxRecordBytes = 256 << 10

// record is one session in the catalogue (version 2). The coordinator keeps
// each execution's launches, visits, receipts, phase, deadline and
// checkpoint; the record keeps what it does not.
//
// First is the first execution's epoch-1 launch, fixed at submission so a
// retried Create sends the same launch; it holds no host path. Executions
// lists the session's executions, the first being the session ID, and
// Current is the one that runs or last ran. CancelOperation is set only when
// the session was cancelled before the coordinator received its first
// execution; afterwards the coordinator keeps cancellation. Finished marks
// the task's tail as written once the coordinator committed its stop.
// Transfer is the pull operation, and Pulled records that a machine claimed
// the session.
type record struct {
	Version          int                  `json:"version"`
	ID               string               `json:"id"`
	Intent           Submit               `json:"intent"`
	DeadlineChoice   string               `json:"deadlineChoice"`
	IntentDigest     string               `json:"intentDigest"`
	First            session.Launch       `json:"first"`
	Association      project.Association  `json:"association,omitzero"`
	RepositorySource *Repository          `json:"repositorySource,omitempty"`
	Executions       []string             `json:"executions"`
	Current          string               `json:"current"`
	CancelOperation  string               `json:"cancelOperation,omitempty"`
	Finished         bool                 `json:"finished,omitempty"`
	Result           string               `json:"result,omitempty"`
	ResultTruncated  bool                 `json:"resultTruncated,omitempty"`
	CheckpointInfo   *checkpoint.Metadata `json:"checkpointInfo,omitempty"`
	Transfer         string               `json:"transfer,omitempty"`
	Pulled           bool                 `json:"pulled,omitempty"`
	Problem          string               `json:"problem,omitempty"`
}

// intentDigest binds a submission and its effective deadline choice: a
// retry under another cap, or with another choice, is a different intent.
func intentDigest(q Submit, choice string) string {
	b, _ := json.Marshal(struct {
		Intent         Submit `json:"intent"`
		DeadlineChoice string `json:"deadlineChoice"`
	}{q, choice})
	return hash(b)
}

// entry is one catalogue record: version 2, or a stopped version 1 record
// the legacy reader serves.
type entry struct {
	current *record
	legacy  *legacyRecord
}

func (e entry) association() project.Association {
	if e.legacy != nil {
		return e.legacy.Association
	}
	return e.current.Association
}

func (o *Owner) path(id, suffix string) string { return filepath.Join(o.config.Directory, id+suffix) }

// ids lists the catalogue's session IDs.
func (o *Owner) ids() ([]string, error) {
	entries, err := os.ReadDir(o.config.Directory)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".json"); ok {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// entry reads id's record in either format.
func (o *Owner) entry(id string) (entry, error) {
	if !CanonicalID(id) {
		return entry{}, &profile.Rejection{Code: profile.CodeInvalid}
	}
	b, err := privatefs.ReadFile(o.path(id, ".json"), maxRecordBytes)
	if os.IsNotExist(err) {
		return entry{}, &profile.Rejection{Code: profile.CodeInvalid}
	}
	if err != nil {
		return entry{}, err
	}
	var probe struct {
		Version int `json:"version"`
	}
	if err = json.Unmarshal(b, &probe); err != nil {
		return entry{}, err
	}
	if probe.Version == legacyVersion {
		l, err := o.readLegacy(id, b)
		return entry{legacy: &l}, err
	}
	r, err := o.decodeRecord(id, b)
	return entry{current: &r}, err
}

// read reads id's version 2 record. A missing record is os.ErrNotExist; a
// version 1 record is refused, since nothing executes it any more.
func (o *Owner) read(id string) (record, error) {
	if !CanonicalID(id) {
		return record{}, &profile.Rejection{Code: profile.CodeInvalid}
	}
	b, err := privatefs.ReadFile(o.path(id, ".json"), maxRecordBytes)
	if err != nil {
		return record{}, err
	}
	return o.decodeRecord(id, b)
}

// decodeRecord checks a record's own invariants: its version, identity,
// intent digest, first launch and executions.
func (o *Owner) decodeRecord(id string, b []byte) (record, error) {
	var r record
	if err := decode(b, &r); err != nil {
		return r, err
	}
	f := r.First
	if r.Version != recordVersion || r.ID != id || o.sessionID(r.Intent.RequestID) != id ||
		r.IntentDigest != intentDigest(r.Intent, r.DeadlineChoice) || f.ExecutionID != id ||
		f.OperationID != r.Intent.RequestID || f.Epoch != 1 || f.ThreadID != "" || f.CheckpointDigest != "" ||
		f.Harness != o.config.Harness || f.Placement != o.config.Placement ||
		f.InstructionDigest != o.config.InstructionDigest || !CanonicalID(f.AllocationID) {
		return r, errors.New("work: retained owner binding changed")
	}
	if len(r.Executions) == 0 || r.Executions[0] != id || !slices.Contains(r.Executions, r.Current) ||
		r.CancelOperation != "" && !CanonicalID(r.CancelOperation) ||
		r.Transfer != "" && !CanonicalID(r.Transfer) || r.Pulled && r.Transfer == "" {
		return r, errors.New("work: retained session record is inconsistent")
	}
	if err := o.checkAssociation(r.Association, r.Intent.Project); err != nil {
		return r, err
	}
	return r, nil
}

func (o *Owner) checkAssociation(a project.Association, intended project.ID) error {
	if a.Template != "" && a.Template != o.config.TemplateName ||
		a.Project != intended || a.Project != "" && !project.ValidID(a.Project) {
		return errors.New("work: retained project binding changed")
	}
	return project.ValidateReference(a.RepositoryReference)
}

func (o *Owner) save(r record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(b) > maxRecordBytes {
		return errors.New("work: session record exceeds its bound")
	}
	return privatefs.Replace(o.config.Directory, r.ID+".json", b)
}

// execution reads the coordinator's record of the session's current
// execution, or nil when the coordinator has not received it.
func (o *Owner) execution(r record) (*session.Record, error) {
	rec, err := o.config.Coordinator.Inspect(r.Current)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// replay answers a repeated submission from its retained session. replayed is
// false only when no session exists yet.
func (o *Owner) replay(id, digest string) (View, bool, error) {
	if _, err := os.Lstat(o.path(id, ".json")); os.IsNotExist(err) {
		return View{}, false, nil
	}
	e, err := o.entry(id)
	if err == nil {
		err = o.access(e.association())
	}
	if err == nil && (e.legacy != nil || e.current.IntentDigest != digest) {
		err = &profile.Rejection{Code: profile.CodeConflict}
	}
	if err != nil {
		return View{}, true, err
	}
	v, err := o.entryView(e)
	return v, true, err
}

func (o *Owner) entryView(e entry) (View, error) {
	if e.legacy != nil {
		return legacyView(*e.legacy), nil
	}
	rec, err := o.execution(*e.current)
	if err != nil {
		return View{}, err
	}
	return o.view(*e.current, rec), nil
}

// info is a session's view without an access check.
func (o *Owner) info(id string) (View, error) {
	e, err := o.entry(id)
	if err != nil {
		return View{}, err
	}
	return o.entryView(e)
}

func (o *Owner) Info(_ context.Context, id string) (View, error) {
	e, err := o.entry(id)
	if err != nil {
		return View{}, err
	}
	if err = o.access(e.association()); err != nil {
		return View{}, err
	}
	return o.entryView(e)
}

func (o *Owner) List(ctx context.Context, q SessionQuery) ([]View, error) {
	if q.Project != "" && !project.ValidID(q.Project) {
		return nil, &profile.Rejection{Code: profile.CodeInvalid}
	}
	if !project.Member(o.config.Projects, q.Project, o.config.Principal) {
		return []View{}, nil
	}
	ids, err := o.ids()
	if err != nil {
		return nil, err
	}
	result := []View{}
	for _, id := range ids {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		e, err := o.entry(id)
		if err != nil {
			return nil, err
		}
		a := e.association()
		if o.access(a) != nil || q.Project != "" && a.Project != q.Project {
			continue
		}
		v, err := o.entryView(e)
		if err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	slices.SortFunc(result, func(a, b View) int { return strings.Compare(a.ID, b.ID) })
	return result, nil
}

// view describes a session from its record and the coordinator's record of
// its current execution, nil when the coordinator has not received it.
func (o *Owner) view(r record, rec *session.Record) View {
	association := r.Association
	if association.Repository == "" {
		association.Repository = r.Intent.Repository
	}
	v := View{Association: association, ID: r.ID, ExecutionID: r.Current, AllocationID: r.First.AllocationID,
		Placement: r.First.Placement, Harness: r.First.Harness, Phase: derivePhase(r, rec),
		Deadline: r.First.Deadline, Login: r.First.Login, Result: r.Result, ResultTruncated: r.ResultTruncated,
		Problem: r.Problem, CheckpointInfo: r.CheckpointInfo, WorkspaceFrom: r.Intent.WorkspaceFrom}
	if rec == nil {
		return v
	}
	v.Deadline, v.ThreadID, v.CheckpointDigest = rec.Deadline, rec.ThreadID, rec.Checkpoint.Metadata.SHA256
	launch, incarnation := latestVisit(*rec)
	if launch != nil {
		v.AllocationID, v.Placement = launch.AllocationID, launch.Placement
	}
	v.Incarnation = incarnation
	if v.Problem == "" {
		v.Problem = executionProblem(*rec)
	}
	return v
}

// latestVisit is the launch and incarnation of the execution's most recent
// visit: the active one, a pending one, or the last stopped one.
func latestVisit(rec session.Record) (*session.Launch, *session.Incarnation) {
	if rec.Active != nil {
		launch, incarnation := rec.Active.Launch, rec.Active.Incarnation
		return &launch, &incarnation
	}
	if rec.PendingOperation != "" {
		if op := rec.Operations[len(rec.Operations)-1]; op.Launch != nil {
			launch := *op.Launch
			var incarnation *session.Incarnation
			if op.Incarnation != nil {
				i := *op.Incarnation
				incarnation = &i
			}
			return &launch, incarnation
		}
	}
	if n := len(rec.Visits); n > 0 {
		launch, incarnation := rec.Visits[n-1].Launch, rec.Visits[n-1].Incarnation
		return &launch, &incarnation
	}
	return nil, nil
}

// executionProblem explains an execution the coordinator could not settle.
func executionProblem(rec session.Record) string {
	switch rec.Phase {
	case session.PhaseUnknownStop:
		return "the placement has not proved the writer stopped; the executor retries, and \"recover\" retries now"
	case session.PhaseUnknownUsage:
		return "the writer stopped, but its usage is not finalized; the executor retries, and \"recover\" retries now"
	}
	return ""
}

// admitRetention refuses a new session once the catalogue holds maxSessions
// records, or once what is retained, plus one checkpoint for every unsettled
// session and for the new one, would exceed RetainedBytes.
func (o *Owner) admitRetention() error {
	ids, err := o.ids()
	if err != nil {
		return err
	}
	if len(ids) >= maxSessions {
		return &profile.Rejection{Code: profile.CodeUnavailable}
	}
	reserved := int64(1)
	for _, id := range ids {
		if v, err := o.info(id); err == nil && !v.Phase.Settled() {
			reserved++
		}
	}
	_, coordinated, err := o.config.Coordinator.Retained()
	if err != nil {
		return err
	}
	var own int64
	err = filepath.WalkDir(o.config.Directory, func(_ string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		info, err := d.Info()
		if err == nil {
			own += info.Size()
		}
		return err
	})
	if err != nil {
		return err
	}
	if coordinated+own+reserved*o.config.MaxCheckpointBytes > o.config.RetainedBytes {
		return &profile.Rejection{Code: profile.CodeUnavailable}
	}
	return nil
}

// lastExit is the exit of the execution's last stopped visit, if its harness
// exited on its own.
func lastExit(rec session.Record) *session.Exit {
	if n := len(rec.Visits); n > 0 {
		return rec.Visits[n-1].Exit
	}
	return nil
}
