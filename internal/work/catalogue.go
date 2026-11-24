package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// the session. Operations binds each owner stop and resume of an interactive
// session to its action and execution, so a replay repeats the same intent.
type record struct {
	Version          int                       `json:"version"`
	ID               string                    `json:"id"`
	Intent           Submit                    `json:"intent"`
	DeadlineChoice   string                    `json:"deadlineChoice"`
	IntentDigest     string                    `json:"intentDigest"`
	First            session.Launch            `json:"first"`
	Association      project.Association       `json:"association,omitzero"`
	RepositorySource *Repository               `json:"repositorySource,omitempty"`
	Executions       []string                  `json:"executions"`
	Current          string                    `json:"current"`
	CancelOperation  string                    `json:"cancelOperation,omitempty"`
	Finished         bool                      `json:"finished,omitempty"`
	Result           string                    `json:"result,omitempty"`
	ResultTruncated  bool                      `json:"resultTruncated,omitempty"`
	CheckpointInfo   *checkpoint.Metadata      `json:"checkpointInfo,omitempty"`
	Transfer         string                    `json:"transfer,omitempty"`
	Pulled           bool                      `json:"pulled,omitempty"`
	Problem          string                    `json:"problem,omitempty"`
	Operations       map[string]boundOperation `json:"operations,omitempty"`
}

// Owner actions bound in record.Operations.
const (
	actionStop   = "stop"
	actionResume = "resume"
)

// maxOperations bounds a session's bound owner operations, and
// maxExecutions its executions, as home's catalogue does.
const (
	maxOperations = 1024
	maxExecutions = 256
)

// boundOperation is one bound owner operation: its action and the execution it
// acts on.
type boundOperation struct {
	Action    string `json:"action"`
	Execution string `json:"execution"`
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

// ids lists the catalogue's session IDs. A session with a removal mark is
// left out, even while its record is still there because the removal is
// unfinished: nothing lists, drives or counts a session being removed.
func (o *Owner) ids() ([]string, error) {
	entries, err := os.ReadDir(o.config.Directory)
	if err != nil {
		return nil, err
	}
	removed := map[string]bool{}
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), removedSuffix); ok {
			removed[id] = true
		}
	}
	var ids []string
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".json"); ok && !removed[id] {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// readRecord reads id's record file. A session with a removal mark reads as
// missing, finished or not, so no action and no driver takes it up again;
// only Remove and Open's finishing of removals look past the mark. A mark
// that cannot be checked is that error: the record is not taken up.
func (o *Owner) readRecord(id string) ([]byte, error) {
	if marked, err := o.removed(id); err != nil {
		return nil, err
	} else if marked {
		return nil, &fs.PathError{Op: "read", Path: o.path(id, ".json"), Err: fs.ErrNotExist}
	}
	return privatefs.ReadFile(o.path(id, ".json"), maxRecordBytes)
}

// recordBytes is readRecord for a command that read the session before:
// a removed session's record, gone or being removed, is invalid, as entry
// answers, so a removal since that read answers as one that came first. Any
// other missing record is os.ErrNotExist.
func (o *Owner) recordBytes(id string) ([]byte, error) {
	b, err := o.readRecord(id)
	if os.IsNotExist(err) && o.wasRemoved(id) {
		return nil, &profile.Rejection{Code: profile.CodeInvalid}
	}
	return b, err
}

// entry reads id's record in either format.
func (o *Owner) entry(id string) (entry, error) {
	if !CanonicalID(id) {
		return entry{}, &profile.Rejection{Code: profile.CodeInvalid}
	}
	b, err := o.readRecord(id)
	if os.IsNotExist(err) {
		return entry{}, &profile.Rejection{Code: profile.CodeInvalid}
	}
	if err != nil {
		return entry{}, err
	}
	return o.decodeEntry(id, b)
}

// decodeEntry decodes the bytes of id's record, in either format.
func (o *Owner) decodeEntry(id string, b []byte) (entry, error) {
	defer o.interleaveAt(stepEntry, id)
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return entry{}, err
	}
	if probe.Version == legacyVersion {
		l, err := o.readLegacy(id, b)
		return entry{legacy: &l}, err
	}
	r, err := o.decodeRecord(id, b)
	return entry{current: &r}, err
}

// read reads id's version 2 record through recordBytes, so a removed
// session's record is invalid and any other missing record is
// os.ErrNotExist. A version 1 record is refused, since nothing executes it
// any more.
func (o *Owner) read(id string) (record, error) {
	if !CanonicalID(id) {
		return record{}, &profile.Rejection{Code: profile.CodeInvalid}
	}
	b, err := o.recordBytes(id)
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
	if err := checkInteractive(r); err != nil {
		return r, err
	}
	if err := o.checkAssociation(r.Association, r.Intent.Project); err != nil {
		return r, err
	}
	return r, nil
}

// checkInteractive checks a record's mode, and that only an interactive
// session has more than one execution and bound owner operations, each
// naming one of its executions.
func checkInteractive(r record) error {
	if len(r.Executions) > maxExecutions || len(r.Operations) > maxOperations ||
		r.Intent.Mode != "" && r.Intent.Mode != ModeInteractive || len(r.Intent.Workspace) > 0 ||
		r.Intent.Mode != ModeInteractive && (len(r.Executions) > 1 || len(r.Operations) > 0) {
		return errors.New("work: retained session record is inconsistent")
	}
	for op, bound := range r.Operations {
		if !CanonicalID(op) || op == r.Intent.RequestID || bound.Action != actionStop && bound.Action != actionResume ||
			!slices.Contains(r.Executions, bound.Execution) {
			return errors.New("work: retained session operation is inconsistent")
		}
	}
	return nil
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
// execution, or nil when the coordinator has not received it. When the
// execution is missing because the session was removed after r was read, it
// answers invalid, as entry does for a removed session, rather than nil,
// which would describe the session as preparing. Remove marks the session
// before it deletes any journal, and the mark stays. A mark that cannot be
// checked is that error: a missing journal then proves neither a removal
// nor an execution not received yet.
func (o *Owner) execution(r record) (*session.Record, error) {
	rec, err := o.config.Coordinator.Inspect(r.Current)
	if os.IsNotExist(err) {
		switch marked, markErr := o.removed(r.ID); {
		case markErr != nil:
			return nil, markErr
		case marked:
			return nil, &profile.Rejection{Code: profile.CodeInvalid}
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// replay answers a repeated submission from its retained session. replayed is
// false only when no session exists yet. A removed session's request is a
// conflict, so it never starts again; a mark that cannot be checked refuses
// with that error.
func (o *Owner) replay(id, digest string) (View, bool, error) {
	if marked, err := o.removed(id); err != nil {
		return View{}, true, err
	} else if marked {
		return View{}, true, &profile.Rejection{Code: profile.CodeConflict}
	}
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
	var v View
	if err == nil {
		v, err = o.entryView(e)
	}
	if err != nil && o.wasRemoved(id) {
		// Removed since the check above: answered as if removed first.
		err = &profile.Rejection{Code: profile.CodeConflict}
	}
	if err != nil {
		return View{}, true, err
	}
	return v, true, nil
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
		var v View
		if err == nil {
			a := e.association()
			if o.access(a) != nil || q.Project != "" && a.Project != q.Project {
				continue
			}
			v, err = o.entryView(e)
		}
		if err != nil && o.wasRemoved(id) {
			// Removed since it was listed: absent, as if removed first.
			continue
		}
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
	v := View{Association: association, ID: r.ID, Mode: r.Intent.Mode, ExecutionID: r.Current, AllocationID: r.First.AllocationID,
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
	if rec.Phase == session.PhaseIdle {
		v.Exit = lastExit(*rec)
	}
	if v.Problem == "" {
		v.Problem = executionProblem(*rec, r.Intent.Mode == ModeInteractive)
	}
	if v.Problem == "" && rec.Phase == session.PhaseIdle && startEnded(*rec) {
		// The driver did not record why the start ended (ADR 0067).
		v.Problem = problemRefusedUnknown
		if o.config.Coordinator.PastDeadline(*rec) {
			v.Problem = problemExpired
		}
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
// The executor retries tasks by itself; an interactive session waits for
// "recover".
func executionProblem(rec session.Record, interactive bool) string {
	retry := "the executor retries, and \"recover\" retries now"
	if interactive {
		retry = "run \"recover\" to retry"
	}
	switch rec.Phase {
	case session.PhaseUnknownStop:
		return "the placement has not proved the writer stopped; " + retry
	case session.PhaseUnknownUsage:
		return "the writer stopped, but its usage is not finalized; " + retry
	}
	return ""
}

// admitRetention admits a new session (newSession) or a new execution of a
// kept one under the retention invariant; the caller holds o.retention until
// it has saved what it admits.
//
// The invariant: RetainedBytes bounds what the executor keeps under work/
// and sessions/ (ADR 0057). Only admission checks it, and admission reserves
// everything the admitted execution may still come to keep, at most
// MaxCheckpointBytes each: the input workspace the coordinator retains when
// it starts (a submission's snapshot, revision or upload) and the checkpoint
// its stop commits. A resume's input is the session's own kept checkpoint or
// workspace, which the coordinator keeps once by digest, so it reserves only
// its checkpoint. An admission is refused unless what is kept now, plus the
// reservation of every admitted execution that may still add to it, plus the
// new one's, fits. A stop, a checkpoint commit, a cancellation and a task's
// tail write only what was reserved and never check the bound: refusing them
// would lose work. A session stops reserving once it is settled, so an
// interactive session reserves its checkpoint for as long as it runs. Records
// and journals are bounded metadata and are not reserved; a kept upload is
// counted twice, as kept and as its session's reserved input.
//
// Two things sit outside the bound. A removed session's mark is not counted
// (see removedSuffix). Scratch space is not reserved: an upload's
// validation before admission, and a driver's capture of a revision or an
// empty workspace after it, each write up to MaxCheckpointBytes under the
// catalogue's directory and delete it when done. While it exists a
// concurrent admission counts it as kept, which only refuses work sooner,
// but the volume may briefly hold that much beyond RetainedBytes.
//
// The catalogue's count, maxSessions, bounds records; a resume adds none.
func (o *Owner) admitRetention(newSession bool) error {
	ids, err := o.ids()
	if err != nil {
		return err
	}
	if newSession && len(ids) >= maxSessions {
		return &profile.Rejection{Code: profile.CodeSessionsFull}
	}
	reserved := int64(1)
	if newSession {
		reserved = 2
	}
	for _, id := range ids {
		n, err := o.reserved(id)
		if err != nil {
			return err
		}
		reserved += n
	}
	_, coordinated, err := o.config.Coordinator.Retained()
	if err != nil {
		return err
	}
	// A removed session's mark is not counted: otherwise an executor at its
	// smallest bound, room for one session, could never admit another after
	// its first removal. Drivers of other sessions write while this sums;
	// privatefs.Usage says what their replacements and scratch contribute.
	own, err := privatefs.Usage(o.config.Directory, func(name string) bool {
		return strings.HasSuffix(name, removedSuffix)
	})
	if err != nil {
		return err
	}
	if coordinated+own+reserved*o.config.MaxCheckpointBytes > o.config.RetainedBytes {
		return &profile.Rejection{Code: profile.CodeRetentionFull}
	}
	return nil
}

// reserved is how many checkpoint-sized reservations session id holds: one
// for the checkpoint an unsettled session may still commit, and one more for
// the input the coordinator has not retained yet. A settled session and a
// version 1 record hold none.
//
// A record or journal that is gone holds none, and so does one that is
// present and read but is not valid: no coordinator operation can load an
// invalid journal, so nothing commits a checkpoint for it, and Remove cannot
// clear it, so failing closed would refuse all work until the volume is
// edited by hand. A record is gone if it was listed and removed before it was
// read, or removed during the read (a removal mark, or a missing record or
// journal), as privatefs.Usage skips an entry removed after its directory was
// listed. A catalogue directory that went too fails admission in Usage, which
// admitRetention calls next.
//
// A record or journal that is present but cannot be read (permission,
// ownership, I/O, too many open files, not a regular file, over its bound, a
// removal mark that cannot be checked, a closed coordinator) fails closed:
// the error is returned and logged, and admission refuses with it, as it does
// when the coordinator's or the catalogue's size cannot be summed. Treating
// "could not look" as "nothing reserved" would admit work the limit refuses.
func (o *Owner) reserved(id string) (int64, error) {
	if !CanonicalID(id) {
		return 0, nil // a stray file, not a session's record
	}
	b, err := o.readRecord(id)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, o.unreserved(id, err)
	}
	e, err := o.decodeEntry(id, b)
	if err != nil || e.legacy != nil {
		return 0, nil // content that is not a valid record
	}
	rec, err := o.execution(*e.current)
	if err != nil {
		var rejection *profile.Rejection
		switch {
		case errors.As(err, &rejection) && rejection.Code == profile.CodeInvalid:
			return 0, nil // removed since the record was read
		case errors.Is(err, session.ErrJournalInvalid):
			return 0, nil // content that is not a valid journal
		}
		return 0, o.unreserved(id, err)
	}
	switch {
	case derivePhase(*e.current, rec).Settled():
		return 0, nil
	case rec == nil:
		return 2, nil
	}
	return 1, nil
}

// unreserved logs a session whose reservation could not be determined and
// returns the error that refuses admission.
func (o *Owner) unreserved(id string, err error) error {
	o.logf("Could not determine reserved storage for work session %s: %v", id, err)
	return fmt.Errorf("work: could not determine reserved storage: %w", err)
}

// lastExit is the exit of the execution's last stopped visit, if its harness
// exited on its own.
func lastExit(rec session.Record) *session.Exit {
	if n := len(rec.Visits); n > 0 {
		return rec.Visits[n-1].Exit
	}
	return nil
}
