// Package work is the remote executor's session catalogue. The executor owns
// each session it starts and drives every execution through the coordinator
// (ADR 0057); the catalogue keeps only what the coordinator does not: the
// submission's intent, its project, its executions, the task's bounded
// result and the transfer to a machine that pulls it. Only the trusted
// installation writes it; artifacts grant no access.
package work

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/deadline"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/google/uuid"
)

// revisionTimeout bounds the in-process restore of a sandbox-produced
// checkpoint when new work starts from a retained revision.
const revisionTimeout = 5 * time.Minute

// maxSessions bounds the catalogue's records.
const maxSessions = 1024

// ProfileStore keeps a submitted profile archive at the placement, which a
// launch then names by digest.
type ProfileStore interface {
	StoreProfile(ctx context.Context, digest string, archive []byte) error
}

// Repository selects an administrator-owned stopped snapshot, or a GitHub URL
// the native worker fetches through its separately approved GitHub capability.
type Repository struct {
	URL       string `json:"url,omitempty"`
	Snapshot  string `json:"snapshot,omitempty"`
	Ref       string `json:"ref,omitempty"`
	Reference string `json:"reference,omitempty"`
}

// Config is the executor's fixed configuration for its catalogue.
// Coordinator owns every execution; its only placement is Placement.
type Config struct {
	Directory         string
	Principal         string
	Harness           harness.NativeKind
	Placement         string
	InstructionDigest string
	Allowances        map[string]session.Allowance
	// MaxDuration caps every deadline; 0 is no cap (ADR 0056).
	MaxDuration time.Duration
	// WorkTokenMaxDuration caps work submitted with the work token, which
	// must have a deadline. It is required and fits within MaxDuration.
	WorkTokenMaxDuration time.Duration
	MaxCheckpointBytes   int64
	// RetainedBytes bounds what the catalogue and the coordinator keep on
	// disk. It is at least twice MaxCheckpointBytes.
	RetainedBytes int64
	Repositories  map[string]Repository
	TemplateName  string
	Projects      map[project.ID]project.Definition
	Coordinator   *session.Coordinator
	Profiles      ProfileStore
	// Terminal is the executor's in-process placement. The catalogue uses
	// it only to attach to an active visit and to see that a harness
	// exited; every lifecycle step goes through Coordinator. Without it the
	// executor refuses interactive sessions.
	Terminal Terminal
	Log      io.Writer

	// retry bounds the driver's automatic recovery of an unproven stop or
	// unsealed usage; zero values take the defaults.
	retry backoff
	// exitInterval is how often the exit sweep looks for interactive
	// sessions whose harness ended; zero takes exitWatchInterval.
	exitInterval time.Duration
}

// Terminal attaches to an active visit's native terminal and reports a
// harness that exited on its own. The Kubernetes placement implements it.
type Terminal interface {
	Attach(context.Context, session.Launch, session.Incarnation, io.Reader, io.Writer) error
	Exited(context.Context, session.Launch, session.Incarnation) (bool, error)
}

// Mode is what a submission starts: a task the executor drives to its
// completion, or an interactive session its owner attaches to, stops and
// resumes.
type Mode string

const (
	// ModeTask is a task; an empty mode means the same.
	ModeTask Mode = "task"
	// ModeInteractive is an interactive session, which only the home
	// principal starts.
	ModeInteractive Mode = "interactive"
)

// Caller is who submitted work: the home principal, or Hermes with the
// work token.
type Caller int

const (
	// CallerHome is the home principal, which Kubernetes authorizes.
	CallerHome Caller = iota
	// CallerWorkToken holds the static work token.
	CallerWorkToken
)

type Submit struct {
	RequestID  string             `json:"requestId"`
	Repository string             `json:"repository"`
	Harness    harness.NativeKind `json:"harness"`
	Executor   string             `json:"executor"`
	// Mode is task (or empty) or interactive. An interactive session has
	// no Task and comes only from the home principal.
	Mode          Mode            `json:"mode,omitempty"`
	Task          string          `json:"task"`
	Project       project.ID      `json:"project,omitempty"`
	WorkspaceFrom *RevisionSource `json:"workspaceFrom,omitempty"`
	// MaxDuration is how long the task may run: empty leaves it to the
	// executor, whose cap for the caller applies when set and otherwise none
	// does; "none" asks for no deadline, which a cap denies; otherwise a
	// positive Go duration within that cap.
	MaxDuration string `json:"maxDuration,omitempty"`
	// Profile is the caller's profile archive. The owner replaces it with
	// ProfileDigest before retaining or comparing the submission; callers never
	// set ProfileDigest themselves.
	Profile       []byte `json:"profile,omitempty"`
	ProfileDigest string `json:"profileDigest,omitempty"`
	// Workspace is a captured workspace archive an interactive session
	// starts from, uploaded by the home principal instead of an approved
	// snapshot; Repository or Project still names what it belongs to. The
	// owner validates it, keeps it until the coordinator retains it, and
	// replaces it with WorkspaceDigest; callers never set WorkspaceDigest.
	Workspace       []byte `json:"workspace,omitempty"`
	WorkspaceDigest string `json:"workspaceDigest,omitempty"`
}

type SessionQuery struct {
	Project project.ID `json:"project,omitempty"`
}

type View struct {
	project.Association
	ID           string               `json:"id"`
	Mode         Mode                 `json:"mode,omitempty"`
	ExecutionID  string               `json:"executionId"`
	AllocationID string               `json:"allocationId"`
	Placement    string               `json:"placement"`
	Incarnation  *session.Incarnation `json:"incarnation,omitempty"`
	Harness      harness.NativeKind   `json:"harness"`
	Phase        Phase                `json:"phase"`
	ThreadID     string               `json:"threadId,omitempty"`
	Deadline     time.Time            `json:"deadline,omitzero"`
	// Login is the named login of a harness that signs in itself.
	Login            string               `json:"login,omitempty"`
	CheckpointDigest string               `json:"checkpointDigest,omitempty"`
	Result           string               `json:"result,omitempty"`
	ResultTruncated  bool                 `json:"resultTruncated,omitempty"`
	Problem          string               `json:"problem,omitempty"`
	CheckpointInfo   *checkpoint.Metadata `json:"checkpointInfo,omitempty"`
	WorkspaceFrom    *RevisionSource      `json:"workspaceFrom,omitempty"`
	// Exit is how the current execution's last visit ended when its
	// harness exited on its own.
	Exit *session.Exit `json:"exit,omitempty"`
}

// Export is transferred over authenticated owner transport. Its source receipt
// is retained history; a claimed home must independently authorize new execution.
type Export struct {
	project.Association
	ID             string               `json:"id"`
	Harness        harness.NativeKind   `json:"harness"`
	Stopped        session.Stopped      `json:"stopped"`
	Checkpoint     []byte               `json:"checkpoint"`
	OperationID    string               `json:"operationId"`
	Claimed        bool                 `json:"claimed"`
	CheckpointInfo *checkpoint.Metadata `json:"checkpointInfo,omitempty"`
}

type Owner struct {
	config Config
	mu     sync.Mutex
	// jobs serializes each session's catalogue record. It is held only for
	// a read, change and save, never while the coordinator waits for a
	// native turn.
	jobs session.JournalLocks
	// retention serializes the retention check with the save it admits.
	retention  sync.Mutex
	lock       *os.File
	ctx        context.Context
	stopAll    context.CancelFunc
	running    map[string]*driver
	wg         sync.WaitGroup
	closeOnce  sync.Once
	closeError error
}

// CanonicalID reports whether s is a non-nil UUID in canonical lowercase form,
// the only spelling accepted for session and operation IDs.
func CanonicalID(s string) bool {
	u, e := uuid.Parse(s)
	return e == nil && u != uuid.Nil && u.String() == s
}
func hash(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
func (o *Owner) sessionID(requestID string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(o.config.Principal+"/"+requestID)).String()
}

// derivedOperation is the operation UUID the owner uses for its own action
// on an execution, the same after every restart.
func derivedOperation(execution, action string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(execution+"/"+action)).String()
}

// Open takes the catalogue's lock, refuses a catalogue that still holds live
// work in the version 1 format, and resumes every unsettled session.
func Open(c Config) (*Owner, error) {
	if c.RetainedBytes == 0 {
		c.RetainedBytes = DefaultRetainedBytes(c.MaxCheckpointBytes)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	if err := privatefs.EnsureDir(c.Directory); err != nil {
		return nil, err
	}
	f, err := privatefs.Lock(filepath.Join(c.Directory, "owner.lock"))
	if err != nil {
		return nil, fmt.Errorf("work: catalogue already owned: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.Allowances = maps.Clone(c.Allowances)
	c.Repositories = maps.Clone(c.Repositories)
	c.Projects = project.Clone(c.Projects)
	o := &Owner{config: c, lock: f, ctx: ctx, stopAll: cancel, running: map[string]*driver{}}
	ids, err := o.ids()
	if err != nil {
		_ = o.Close()
		return nil, err
	}
	var live []string
	var resume []string
	for _, id := range ids {
		e, readErr := o.entry(id)
		if readErr != nil {
			_ = o.Close()
			return nil, fmt.Errorf("work: session %s: %w", id, readErr)
		}
		switch {
		case e.legacy != nil && !e.legacy.Phase.stoppedOrCancelled():
			live = append(live, fmt.Sprintf("%s (%s)", id, e.legacy.Phase))
		case e.legacy == nil:
			resume = append(resume, id)
		}
	}
	if len(live) > 0 {
		_ = o.Close()
		return nil, fmt.Errorf("work: the catalogue holds unfinished sessions in the version 1 format: %s; "+
			"run the previous release until each is retained or cancelled (\"sproozi cancel ID --remote\"), "+
			"then start this release", strings.Join(live, ", "))
	}
	o.removeKeptUploads()
	for _, id := range resume {
		if v, err := o.info(id); err == nil && !v.Phase.Settled() {
			o.dispatch(id)
		}
	}
	if c.Terminal != nil {
		o.wg.Go(func() { o.watchExits(ctx) })
	}
	return o, nil
}

// DefaultRetainedBytes is the retention bound when the executor sets none:
// 1 GiB, or twice the checkpoint limit when that is larger.
func DefaultRetainedBytes(maxCheckpointBytes int64) int64 {
	return max(1<<30, 2*maxCheckpointBytes)
}

func (o *Owner) Submit(ctx context.Context, q Submit) (View, error) {
	return o.SubmitAs(ctx, CallerHome, q)
}

// SubmitAs starts work for caller. Work submitted with the work token takes
// the executor's work-token cap on its deadline.
func (o *Owner) SubmitAs(ctx context.Context, caller Caller, q Submit) (View, error) {
	if ctx.Err() != nil {
		return View{}, ctx.Err()
	}
	upload, err := o.checkMode(caller, &q)
	if err != nil {
		return View{}, err
	}
	interactive := q.Mode == ModeInteractive
	if q.Harness != o.config.Harness || !interactive && !runsTasks(q.Harness) {
		return View{}, &profile.Rejection{Code: profile.CodeUnsupported}
	}
	if q.Executor != o.config.Placement {
		return View{}, &profile.Rejection{Code: profile.CodeDenied}
	}
	now := time.Now()
	choice, until, err := o.taskDeadline(caller, q.MaxDuration, now)
	if err != nil {
		return View{}, err
	}
	archive := q.Profile
	if q.ProfileDigest != "" {
		return View{}, &profile.Rejection{Code: profile.CodeInvalid}
	}
	if len(archive) > 0 {
		sum := sha256.Sum256(archive)
		q.Profile, q.ProfileDigest = nil, "sha256:"+hex.EncodeToString(sum[:])
		if agentprofile.Verify(q.ProfileDigest, archive) != nil {
			return View{}, &profile.Rejection{Code: profile.CodeInvalid}
		}
	}
	id := o.sessionID(q.RequestID)
	digest := intentDigest(q, choice)
	// Submission replay is a metadata read, not a wait for the active native turn.
	// Check again under the lock before creating the session.
	if v, replayed, err := o.replay(id, digest); replayed {
		return v, err
	}
	unlock := o.jobs.Lock(id)
	defer unlock()
	if v, replayed, err := o.replay(id, digest); replayed {
		return v, err
	}
	association, repository, allowances, err := o.resolve(q)
	if err != nil {
		return View{}, err
	}
	if q.ProfileDigest != "" {
		if err = o.config.Profiles.StoreProfile(ctx, q.ProfileDigest, archive); err != nil {
			return View{}, err
		}
	}
	if q.WorkspaceFrom != nil {
		if _, err = o.revisionSource(*q.WorkspaceFrom, association); err != nil {
			return View{}, err
		}
	}
	if len(upload) > 0 {
		if err = o.validateUpload(ctx, upload); err != nil {
			return View{}, err
		}
	}
	o.retention.Lock()
	defer o.retention.Unlock()
	if err = o.admitRetention(); err != nil {
		return View{}, err
	}
	if len(upload) > 0 {
		// Kept until the coordinator retains it as the first visit's input.
		if err = privatefs.Replace(o.config.Directory, id+uploadSuffix, upload); err != nil {
			return View{}, err
		}
	}
	// The task is data for the native agent; credentials and scope remain at
	// the gateway. It is fixed here, because a launch never changes.
	task := q.Task
	if !interactive && q.WorkspaceFrom == nil && repository.Snapshot == "" && repository.URL != "" {
		task = fmt.Sprintf("Work in the current controlled workspace. Fetch repository %s through the configured gateway and check out ref %q before performing this task. Do not copy host credentials.\n\n%s", repository.URL, repository.Ref, q.Task)
	}
	r := record{Version: recordVersion, ID: id, Intent: q, DeadlineChoice: choice, IntentDigest: digest,
		Association: association, RepositorySource: &repository, Executions: []string{id}, Current: id,
		First: session.Launch{Harness: q.Harness, ExecutionID: id, OperationID: q.RequestID,
			AllocationID: uuid.NewString(), Epoch: 1, Placement: q.Executor, InstructionDigest: o.config.InstructionDigest,
			Deadline: until.Time().UTC(), Allowances: allowances, Task: task, ProfileDigest: q.ProfileDigest}}
	if err = o.save(r); err != nil {
		return View{}, err
	}
	o.dispatch(id)
	return o.view(r, nil), nil
}

// checkMode checks a submission's mode, task and upload for caller. It
// normalizes the task mode to empty and replaces an upload with its digest,
// returning the upload.
func (o *Owner) checkMode(caller Caller, q *Submit) ([]byte, error) {
	if q.Mode == ModeTask {
		q.Mode = ""
	}
	interactive := q.Mode == ModeInteractive
	if !CanonicalID(q.RequestID) || q.Mode != "" && !interactive {
		return nil, &profile.Rejection{Code: profile.CodeInvalid}
	}
	if interactive && caller != CallerHome {
		return nil, &profile.Rejection{Code: profile.CodeDenied}
	}
	if interactive && q.Task != "" || !interactive && (strings.TrimSpace(q.Task) == "" || len(q.Task) > 16<<10 ||
		!utf8.ValidString(q.Task) || strings.ContainsRune(q.Task, '\x00')) {
		return nil, &profile.Rejection{Code: profile.CodeInvalid}
	}
	if interactive && o.config.Terminal == nil {
		return nil, &profile.Rejection{Code: profile.CodeUnsupported}
	}
	upload := q.Workspace
	if q.WorkspaceDigest != "" || len(upload) > 0 && (!interactive || q.WorkspaceFrom != nil ||
		int64(len(upload)) > o.config.MaxCheckpointBytes) {
		return nil, &profile.Rejection{Code: profile.CodeInvalid}
	}
	if len(upload) > 0 {
		sum := sha256.Sum256(upload)
		q.Workspace, q.WorkspaceDigest = nil, "sha256:"+hex.EncodeToString(sum[:])
	}
	return upload, nil
}

// taskDeadline is the deadline a submission asks for (ADR 0056) and the
// effective choice the intent records: a duration, or none. Without a choice
// the caller's cap applies, or no deadline when there is none. A chosen
// duration, or none, must fit that cap. The work token's cap is
// WorkTokenMaxDuration, which is always set.
func (o *Owner) taskDeadline(caller Caller, choice string, now time.Time) (string, deadline.Deadline, error) {
	limit := o.config.MaxDuration
	if caller == CallerWorkToken {
		limit = o.config.WorkTokenMaxDuration
	}
	d := limit
	if choice != "" {
		var err error
		if d, err = deadline.ParseMaxDuration(choice); err != nil {
			return "", deadline.None(), &profile.Rejection{Code: profile.CodeInvalid}
		}
	}
	until := deadline.After(now, d)
	if !until.Fits(now, limit) {
		return "", deadline.None(), &profile.Rejection{Code: profile.CodeDenied}
	}
	if d == 0 {
		return deadline.NoneArg, until, nil
	}
	return d.String(), until, nil
}

// Recover retries a session's unfinished step now: an unproven stop or
// unsealed usage, a stop still waiting for the task's turn, or a preparation
// that failed. It never starts a session again that is stopped or cancelled.
func (o *Owner) Recover(ctx context.Context, id string) (View, error) {
	if ctx.Err() != nil {
		return View{}, ctx.Err()
	}
	e, err := o.entry(id)
	if err != nil {
		return View{}, err
	}
	if err = o.access(e.association()); err != nil {
		return View{}, err
	}
	v, err := o.entryView(e)
	if err != nil {
		return View{}, err
	}
	if e.legacy == nil && e.current.Intent.Mode == ModeInteractive {
		return o.recoverInteractive(ctx, *e.current, v)
	}
	if e.legacy == nil && !v.Phase.Settled() {
		o.wake(id)
	}
	return v, nil
}

// errCancelledBeforeStart ends a cancellation that the catalogue settled
// itself, because the coordinator never received the execution.
var errCancelledBeforeStart = errors.New("work: cancelled before it started")

// Cancel cancels the session's current execution through the coordinator,
// which first interrupts the driver's graceful stop, so cancellation never
// waits for a native turn. A session already stopped is refused: its result
// is kept.
func (o *Owner) Cancel(ctx context.Context, id, operation string) (View, error) {
	return o.CancelAs(ctx, CallerHome, id, operation)
}

// CancelAs cancels for caller. Only the home principal cancels an
// interactive session.
func (o *Owner) CancelAs(ctx context.Context, caller Caller, id, operation string) (View, error) {
	if !CanonicalID(operation) {
		return View{}, &profile.Rejection{Code: profile.CodeInvalid}
	}
	e, err := o.entry(id)
	if err != nil {
		return View{}, err
	}
	if err = o.access(e.association()); err != nil {
		return View{}, err
	}
	if e.current != nil && e.current.Intent.Mode == ModeInteractive && caller != CallerHome {
		return View{}, &profile.Rejection{Code: profile.CodeDenied}
	}
	if e.legacy != nil {
		return o.cancelLegacy(*e.legacy, operation)
	}
	if err = o.cancelExecution(ctx, id, operation, ""); err != nil {
		return View{}, err
	}
	return o.Info(ctx, id)
}

// cancelExecution carries out cancellation op of the session's current execution and,
// once it is retained, records problem. A session the coordinator never
// received is cancelled in the catalogue alone.
func (o *Owner) cancelExecution(ctx context.Context, id, op, problem string) error {
	r, err := o.read(id)
	if err != nil {
		return err
	}
	var previous string
	admit := func() (func(), error) {
		unlock := o.jobs.Lock(id)
		current, err := o.read(id)
		if err != nil {
			unlock()
			return nil, err
		}
		if current.Transfer != "" {
			unlock()
			return nil, &profile.Rejection{Code: profile.CodeConflict}
		}
		if current.CancelOperation != "" {
			unlock()
			if current.CancelOperation != op {
				return nil, &profile.Rejection{Code: profile.CodeConflict}
			}
			return nil, errCancelledBeforeStart
		}
		if _, err = o.config.Coordinator.Inspect(current.Current); os.IsNotExist(err) {
			current.CancelOperation, current.Problem = op, problem
			err = o.save(current)
			if err == nil && current.Intent.WorkspaceDigest != "" {
				o.removeUpload(id)
			}
			unlock()
			if err != nil {
				return nil, err
			}
			return nil, errCancelledBeforeStart
		} else if err != nil {
			unlock()
			return nil, err
		}
		if problem != "" && current.Problem != problem {
			previous = current.Problem
			current.Problem = problem
			if err = o.save(current); err != nil {
				unlock()
				return nil, err
			}
		}
		return unlock, nil
	}
	_, err = o.config.Coordinator.CancelUnsettled(ctx, r.Current, op, admit)
	switch {
	case errors.Is(err, errCancelledBeforeStart):
		return nil
	case errors.Is(err, session.ErrAlreadyStopped):
		// The stop committed first: the session keeps its result, so the
		// problem recorded for the cancellation no longer applies.
		if problem != "" {
			o.restoreProblem(id, problem, previous)
		}
		return &profile.Rejection{Code: profile.CodeConflict}
	case errors.Is(err, session.ErrSessionCancelled), errors.Is(err, session.ErrCancellationUnfinished):
		return &profile.Rejection{Code: profile.CodeConflict}
	}
	return err
}

// restoreProblem puts back the problem a refused cancellation replaced.
func (o *Owner) restoreProblem(id, replaced, previous string) {
	unlock := o.jobs.Lock(id)
	defer unlock()
	if r, err := o.read(id); err == nil && r.Problem == replaced {
		r.Problem = previous
		if err = o.save(r); err != nil {
			o.logf("Could not record work session %s: %v", id, err)
		}
	}
}

func (o *Owner) Close() error {
	o.closeOnce.Do(func() {
		o.stopAll()
		o.mu.Lock()
		for _, d := range o.running {
			d.stop()
		}
		o.mu.Unlock()
		o.wg.Wait()
		o.closeError = privatefs.UnlockClose(o.lock)
	})
	return o.closeError
}

// validateLimits checks the work token's cap and the retention bound.
func (c Config) validateLimits() error {
	if c.WorkTokenMaxDuration <= 0 || c.MaxDuration > 0 && c.WorkTokenMaxDuration > c.MaxDuration {
		return errors.New("work: the work token's maximum duration is required and must fit the executor's maxDuration")
	}
	if c.RetainedBytes < 2*c.MaxCheckpointBytes {
		return errors.New("work: retained bytes must be at least twice the checkpoint limit")
	}
	return nil
}

// executorHarness reports whether an executor may run kind: Codex runs tasks
// and interactive sessions, Claude Code interactive sessions only.
func executorHarness(kind harness.NativeKind) bool {
	return kind == harness.NativeCodex || kind == harness.NativeClaude
}

// runsTasks reports whether an executor for kind runs tasks: only Codex,
// until the supervisor binds a Claude task's completion (ADR 0057).
func runsTasks(kind harness.NativeKind) bool {
	return kind == harness.NativeCodex
}

func (c Config) validate() error {
	if !filepath.IsAbs(c.Directory) || c.Principal == "" || !executorHarness(c.Harness) || c.Placement == "" || len(c.InstructionDigest) != 64 || c.MaxDuration < 0 || c.MaxCheckpointBytes <= 0 || c.MaxCheckpointBytes > 64<<20 || c.Coordinator == nil || c.Profiles == nil || len(c.Repositories)+len(c.Projects) == 0 || len(c.Allowances) == 0 {
		return errors.New("work: fixed owner configuration required")
	}
	if err := c.validateLimits(); err != nil {
		return err
	}
	for name, r := range c.Repositories {
		if name == "" || strings.ContainsAny(name, "/\\\x00") || (r.Snapshot == "" && !strings.HasPrefix(r.URL, "https://github.com/")) || (r.Snapshot != "" && !filepath.IsAbs(r.Snapshot)) {
			return errors.New("work: approved repository name and source required")
		}
		if err := project.ValidateReference(r.Reference); err != nil {
			return err
		}
		if err := project.ValidateReference(r.URL); err != nil {
			return err
		}
	}
	for id, definition := range c.Projects {
		r := c.Repositories[definition.Workspace]
		if definition.Workspace != "" && r.Reference == "" && r.URL == "" {
			return fmt.Errorf("work: project %q snapshot workspace requires a credential-free repository reference", id)
		}
	}
	repositories := map[string]bool{}
	for name := range c.Repositories {
		repositories[name] = true
	}
	return project.Validate(c.Projects, c.TemplateName, repositories, c.Allowances)
}
