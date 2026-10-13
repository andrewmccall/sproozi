// Package work owns remote task sessions independently of worker lifetimes.
// Only the trusted installation writes its catalogue; artifacts grant no access.
package work

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/workspace"
	"github.com/google/uuid"
)

const (
	quietArg           = "--quiet"
	phaseActive        = "active"
	phaseCancelled     = "cancelled"
	phaseCancelling    = "cancelling"
	phaseExporting     = "exporting"
	phaseFinalizing    = "finalizing"
	phaseHomeOwned     = "home-owned"
	executorKubernetes = "kubernetes"
	phasePending       = "pending"
	phasePreparing     = "preparing"
	phaseRetained      = "retained"
	repositorySproozi  = "sproozi"
	phaseUnknown       = "unknown"
)

type InitialPlacement interface {
	session.Placement
	CreateInitial(context.Context, session.Launch, []byte) (session.Incarnation, error)
}

// Repository selects an administrator-owned stopped snapshot, or a GitHub URL
// the native worker fetches through its separately approved GitHub capability.
type Repository struct {
	URL      string `json:"url,omitempty"`
	Snapshot string `json:"snapshot,omitempty"`
	Ref      string `json:"ref,omitempty"`
}

type Config struct {
	Directory          string
	Principal          string
	Harness            harness.NativeKind
	Placement          string
	InstructionDigest  string
	Allowances         map[string]session.Allowance
	MaxDuration        time.Duration
	MaxCheckpointBytes int64
	Repositories       map[string]Repository
	Backend            InitialPlacement
	Log                io.Writer
}

type Submit struct {
	RequestID  string             `json:"requestId"`
	Repository string             `json:"repository"`
	Harness    harness.NativeKind `json:"harness"`
	Executor   string             `json:"executor"`
	Task       string             `json:"task"`
}

type View struct {
	ID               string             `json:"id"`
	ExecutionID      string             `json:"executionId"`
	Harness          harness.NativeKind `json:"harness"`
	Repository       string             `json:"repository"`
	Phase            string             `json:"phase"`
	ThreadID         string             `json:"threadId,omitempty"`
	Deadline         time.Time          `json:"deadline"`
	CheckpointDigest string             `json:"checkpointDigest,omitempty"`
	Result           string             `json:"result,omitempty"`
	ResultTruncated  bool               `json:"resultTruncated,omitempty"`
	Problem          string             `json:"problem,omitempty"`
}

// Export is transferred over authenticated owner transport. Its source receipt
// is retained history; a claimed home must independently authorize new execution.
type Export struct {
	ID          string             `json:"id"`
	Harness     harness.NativeKind `json:"harness"`
	Stopped     session.Stopped    `json:"stopped"`
	Checkpoint  []byte             `json:"checkpoint"`
	OperationID string             `json:"operationId"`
	Claimed     bool               `json:"claimed"`
}

type record struct {
	Version         int                  `json:"version"`
	Intent          Submit               `json:"intent"`
	IntentDigest    string               `json:"intentDigest"`
	Launch          session.Launch       `json:"launch"`
	Phase           string               `json:"phase"`
	Progress        string               `json:"progress,omitempty"`
	Incarnation     *session.Incarnation `json:"incarnation,omitempty"`
	Stopped         *session.Stopped     `json:"stopped,omitempty"`
	InputDigest     string               `json:"inputDigest,omitempty"`
	Result          string               `json:"result,omitempty"`
	ResultTruncated bool                 `json:"resultTruncated,omitempty"`
	Problem         string               `json:"problem,omitempty"`
	Transfer        string               `json:"transfer,omitempty"`
	CancelOperation string               `json:"cancelOperation,omitempty"`
}

type Owner struct {
	config     Config
	mu         sync.Mutex
	jobs       session.JournalLocks
	lock       *os.File
	ctx        context.Context
	cancel     context.CancelFunc
	running    map[string]context.CancelFunc
	wg         sync.WaitGroup
	closeOnce  sync.Once
	closeError error
}

func canonicalID(s string) bool {
	u, e := uuid.Parse(s)
	return e == nil && u != uuid.Nil && u.String() == s
}
func hash(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
func (o *Owner) sessionID(requestID string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(o.config.Principal+"/"+requestID)).String()
}

func Open(c Config) (*Owner, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(c.Directory, 0700); err != nil {
		return nil, err
	}
	if err := private(c.Directory, true); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(c.Directory, "owner.lock")
	if _, err := os.Lstat(lockPath); err == nil {
		if err = private(lockPath, false); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, errors.New("work: catalogue already owned")
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.Allowances = maps.Clone(c.Allowances)
	c.Repositories = maps.Clone(c.Repositories)
	o := &Owner{config: c, lock: f, ctx: ctx, cancel: cancel, running: map[string]context.CancelFunc{}}
	entries, err := os.ReadDir(c.Directory)
	if err != nil {
		_ = o.Close()
		return nil, err
	}
	for _, e := range entries {
		if before, ok := strings.CutSuffix(e.Name(), ".json"); ok {
			r, readErr := o.read(before)
			if readErr != nil {
				_ = o.Close()
				return nil, readErr
			}
			if r.Phase == phasePending || r.Phase == phasePreparing || r.Phase == phaseActive || r.Phase == phaseFinalizing || r.Phase == phaseCancelling {
				o.dispatch(r.Launch.SessionID)
			}
		}
	}
	return o, nil
}

func private(path string, dir bool) error {
	i, e := os.Lstat(path)
	if e != nil {
		return e
	}
	s, ok := i.Sys().(*syscall.Stat_t)
	if !ok || i.IsDir() != dir || i.Mode()&os.ModeSymlink != 0 || (!dir && !i.Mode().IsRegular()) || i.Mode().Perm()&0077 != 0 || s.Uid != uint32(os.Geteuid()) {
		return errors.New("work: owned private path required")
	}
	return nil
}
func (o *Owner) path(id, suffix string) string { return filepath.Join(o.config.Directory, id+suffix) }
func (o *Owner) read(id string) (record, error) {
	var r record
	if !canonicalID(id) {
		return r, errors.New("work: canonical session UUID required")
	}
	p := o.path(id, ".json")
	if err := private(p, false); err != nil {
		return r, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return r, err
	}
	if len(b) > 256<<10 {
		return r, errors.New("work: catalogue record exceeds limit")
	}
	if err = decode(b, &r); err != nil {
		return r, err
	}
	intent, _ := json.Marshal(r.Intent)
	if r.Version != 1 || o.sessionID(r.Intent.RequestID) != id || r.Launch.SessionID != id || r.IntentDigest != hash(intent) || r.Launch.Harness != o.config.Harness || r.Launch.Placement != o.config.Placement || r.Launch.InstructionDigest != o.config.InstructionDigest || !canonicalID(r.Launch.OperationID) || !canonicalID(r.Launch.AllocationID) {
		return r, errors.New("work: retained owner binding changed")
	}
	return r, nil
}
func atomic(dir, name string, b []byte) error {
	f, e := os.CreateTemp(dir, ".pending-")
	if e != nil {
		return e
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	e = errors.Join(e, f.Close())
	if e != nil {
		return e
	}
	if e = os.Rename(f.Name(), filepath.Join(dir, name)); e != nil {
		return e
	}
	parent, e := os.Open(dir)
	if e != nil {
		return e
	}
	return errors.Join(parent.Sync(), parent.Close())
}
func (o *Owner) save(r record) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	current, err := o.read(r.Launch.SessionID)
	if err == nil && current.CancelOperation != "" && r.CancelOperation == "" {
		return errors.New("work: cancellation wins")
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return atomic(o.config.Directory, r.Launch.SessionID+".json", b)
}
func (o *Owner) Submit(ctx context.Context, q Submit) (View, error) {
	if ctx.Err() != nil {
		return View{}, ctx.Err()
	}
	if !canonicalID(q.RequestID) || q.Harness != o.config.Harness || q.Executor != o.config.Placement || strings.TrimSpace(q.Task) == "" || len(q.Task) > 16<<10 || !utf8.ValidString(q.Task) || strings.ContainsRune(q.Task, '\x00') {
		return View{}, errors.New("work: invalid or unsupported task intent")
	}
	if _, ok := o.config.Repositories[q.Repository]; !ok {
		return View{}, errors.New("work: repository not approved")
	}
	id := o.sessionID(q.RequestID)
	b, _ := json.Marshal(q)
	digest := hash(b)
	// Submission replay is a metadata read, not a wait for the active native turn.
	if current, readErr := o.read(id); readErr == nil {
		if current.IntentDigest != digest {
			return View{}, errors.New("work: submission intent conflicts")
		}
		return view(current), nil
	} else if !os.IsNotExist(readErr) {
		return View{}, readErr
	}
	unlock := o.jobs.Lock(id)
	defer unlock()
	r, err := o.read(id)
	if err == nil {
		if r.IntentDigest != digest {
			return View{}, errors.New("work: submission intent conflicts")
		}
		return view(r), nil
	}
	if !os.IsNotExist(err) {
		return View{}, err
	}
	entries, err := os.ReadDir(o.config.Directory)
	if err != nil {
		return View{}, err
	}
	if len(entries) > 1024 {
		return View{}, errors.New("work: catalogue retention limit reached")
	}
	r = record{Version: 1, Intent: q, IntentDigest: digest, Phase: phasePending, Launch: session.Launch{Harness: q.Harness, SessionID: id, OperationID: q.RequestID, AllocationID: uuid.NewString(), Epoch: 1, Placement: q.Executor, InstructionDigest: o.config.InstructionDigest, Deadline: time.Now().Add(o.config.MaxDuration).UTC(), Allowances: maps.Clone(o.config.Allowances), Task: q.Task}}
	if err = o.save(r); err != nil {
		return View{}, err
	}
	o.dispatch(id)
	return view(r), nil
}
func view(r record) View {
	v := View{ID: r.Launch.SessionID, ExecutionID: r.Launch.AllocationID, Harness: r.Launch.Harness, Repository: r.Intent.Repository, Phase: r.Phase, Deadline: r.Launch.Deadline, Result: r.Result, ResultTruncated: r.ResultTruncated, Problem: r.Problem}
	if r.Stopped != nil {
		v.ThreadID = r.Stopped.Launch.ThreadID
		v.CheckpointDigest = r.Stopped.CheckpointDigest
	}
	return v
}
func (o *Owner) Info(_ context.Context, id string) (View, error) {
	r, e := o.read(id)
	return view(r), e
}
func (o *Owner) List(ctx context.Context) ([]View, error) {
	entries, e := os.ReadDir(o.config.Directory)
	if e != nil {
		return nil, e
	}
	result := []View{}
	for _, f := range entries {
		if !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		r, e := o.read(strings.TrimSuffix(f.Name(), ".json"))
		if e != nil {
			return nil, e
		}
		result = append(result, view(r))
	}
	slices.SortFunc(result, func(a, b View) int { return strings.Compare(a.ID, b.ID) })
	return result, nil
}
func (o *Owner) dispatch(id string) {
	o.mu.Lock()
	if o.ctx.Err() != nil || o.running[id] != nil {
		o.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(o.ctx)
	o.running[id] = cancel
	o.wg.Add(1)
	o.mu.Unlock()
	go func() {
		defer o.wg.Done()
		defer func() { cancel(); o.mu.Lock(); delete(o.running, id); o.mu.Unlock() }()
		unlock := o.jobs.Lock(id)
		defer unlock()
		o.drive(ctx, id)
	}()
}
func (o *Owner) drive(ctx context.Context, id string) {
	r, err := o.read(id)
	if err != nil {
		return
	}
	if r.CancelOperation != "" {
		o.abort(ctx, r)
		return
	}
	err = o.execute(ctx, &r)
	if err != nil && o.ctx.Err() == nil {
		if o.config.Log != nil {
			_, _ = fmt.Fprintf(o.config.Log, "Could not complete work session %s at %s: %v\n", id, r.Phase, err)
		}
		latest, readErr := o.read(id)
		if readErr != nil {
			return
		}
		if latest.CancelOperation != "" {
			o.abort(o.ctx, latest)
			return
		}
		latest.Problem = "Execution requires recovery; no writer or usage assumption was made"
		latest.Progress = latest.Phase
		latest.Phase = phaseUnknown
		_ = o.save(latest)
	}
}
func (o *Owner) input(ctx context.Context, r *record) ([]byte, error) {
	p := o.path(r.Launch.SessionID, ".input")
	if r.InputDigest != "" {
		if e := private(p, false); e != nil {
			return nil, e
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return nil, e
		}
		if hash(b) != r.InputDigest {
			return nil, errors.New("work: input digest changed")
		}
		return b, nil
	}
	repo := o.config.Repositories[r.Intent.Repository]
	var b []byte
	var err error
	if repo.Snapshot != "" {
		b, err = workspace.Capture(ctx, repo.Snapshot, nil, o.config.MaxCheckpointBytes)
	} else {
		dir, e := os.MkdirTemp(o.config.Directory, ".seed-")
		if e != nil {
			return nil, e
		}
		defer func() { _ = os.RemoveAll(dir) }()
		for _, args := range [][]string{{"init", quietArg, "--template=", dir}, {"-C", dir, "-c", "user.name=Sproozi", "-c", "user.email=bootstrap@sproozi.invalid", "commit", quietArg, "--allow-empty", "-m", "Initialize controlled workspace"}} {
			cmd := exec.CommandContext(ctx, "git", args...)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
			if out, e := cmd.CombinedOutput(); e != nil {
				return nil, fmt.Errorf("work: seed repository: %w: %.1024s", e, out)
			}
		}
		b, err = workspace.Capture(ctx, dir, nil, o.config.MaxCheckpointBytes)
		// The task is data for the native agent; credentials and scope remain at the gateway.
		r.Launch.Task = fmt.Sprintf("Work in the current controlled workspace. Fetch repository %s through the configured gateway and check out ref %q before performing this task. Do not copy host credentials.\n\n%s", repo.URL, repo.Ref, r.Intent.Task)
	}
	if err != nil {
		return nil, err
	}
	if err = atomic(o.config.Directory, r.Launch.SessionID+".input", b); err != nil {
		return nil, err
	}
	r.InputDigest = hash(b)
	if err = o.save(*r); err != nil {
		return nil, err
	}
	return b, nil
}
func (o *Owner) execute(ctx context.Context, r *record) error {
	if r.Phase == phasePending || r.Phase == phasePreparing {
		if !time.Now().Before(r.Launch.Deadline) {
			return errors.New("work: unstarted task expired")
		}
		b, e := o.input(ctx, r)
		if e != nil {
			return e
		}
		r.Phase = phasePreparing
		if e = o.save(*r); e != nil {
			return e
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		inc, e := o.config.Backend.CreateInitial(ctx, r.Launch, b)
		if e != nil {
			return e
		}
		r.Incarnation = &inc
		r.Phase = phaseActive
		if e = o.save(*r); e != nil {
			return e
		}
	}
	if r.Phase == phaseActive {
		if r.Incarnation == nil {
			return errors.New("work: exact incarnation unavailable")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e := o.config.Backend.Activate(ctx, r.Launch, *r.Incarnation); e != nil {
			return e
		}
		r.Phase = phaseFinalizing
		if e := o.save(*r); e != nil {
			return e
		}
	}
	if r.Phase == phaseFinalizing {
		if r.Incarnation == nil {
			return errors.New("work: exact incarnation unavailable")
		}
		stopped, e := o.config.Backend.Stop(ctx, r.Launch, *r.Incarnation)
		if e != nil {
			return e
		}
		if e = ValidateExport(Export{ID: r.Launch.SessionID, Harness: r.Launch.Harness, Stopped: stopped, Checkpoint: stopped.Checkpoint}, o.config.MaxCheckpointBytes); e != nil {
			return e
		}
		if stopped.Launch.OperationID != r.Launch.OperationID || stopped.Launch.AllocationID != r.Launch.AllocationID || stopped.Incarnation != *r.Incarnation {
			return errors.New("work: stopped execution binding mismatch")
		}
		bound := stopped.Launch
		bound.ThreadID = r.Launch.ThreadID
		expected, _ := json.Marshal(r.Launch)
		actual, _ := json.Marshal(bound)
		if !bytes.Equal(expected, actual) {
			return errors.New("work: stopped launch intent changed")
		}
		if e = atomic(o.config.Directory, r.Launch.SessionID+".checkpoint", stopped.Checkpoint); e != nil {
			return e
		}
		r.Result, r.ResultTruncated, e = checkpoint.FinalText(stopped.Checkpoint, o.config.MaxCheckpointBytes)
		if e != nil {
			return e
		}
		stopped.Checkpoint = nil
		r.Stopped = &stopped
		r.Phase = phaseRetained
		r.Problem = ""
		return o.save(*r)
	}
	return nil
}

// ValidateExport keeps transcript validation separate from trusted stop/seal evidence.
func ValidateExport(e Export, max int64) error {
	s := e.Stopped
	if !canonicalID(e.ID) || s.Launch.SessionID != e.ID || e.Harness != s.Launch.Harness || !s.ActualStopped || s.Receipt == nil || s.Receipt.Incarnation != s.Incarnation || s.Receipt.Launch.ThreadID != s.Launch.ThreadID || len(s.Receipt.Usage) != len(s.Launch.Allowances) {
		return errors.New("work: trusted writer fence and complete accounting required")
	}
	for cap, a := range s.Launch.Allowances {
		u, ok := s.Receipt.Usage[cap]
		if !ok || u.Units < 0 || u.CostMicros < 0 || a.Units.Bounded && u.Units > a.Units.Value || a.CostMicros.Bounded && u.CostMicros > a.CostMicros.Value {
			return errors.New("work: invalid source usage")
		}
	}
	a, _ := json.Marshal(s.Launch)
	b, _ := json.Marshal(s.Receipt.Launch)
	if string(a) != string(b) {
		return errors.New("work: source receipt intent differs")
	}
	meta, err := checkpoint.Inspect(e.Checkpoint, max)
	if err != nil {
		return err
	}
	if meta.Harness != e.Harness || meta.ThreadID != s.Launch.ThreadID || meta.SHA256 != s.CheckpointDigest {
		return errors.New("work: retained checkpoint binding differs")
	}
	return checkpoint.ValidateContinuity(e.Checkpoint, e.Harness, s.Launch.InstructionDigest, max)
}
func (o *Owner) BeginExport(ctx context.Context, id, operation string) (Export, error) {
	return o.export(ctx, id, operation, "", false)
}
func (o *Owner) Claim(ctx context.Context, id, operation, digest string) (Export, error) {
	return o.export(ctx, id, operation, digest, true)
}
func (o *Owner) export(ctx context.Context, id, operation, digest string, claim bool) (Export, error) {
	if !canonicalID(id) || !canonicalID(operation) {
		return Export{}, errors.New("work: canonical transfer UUIDs required")
	}
	if ctx.Err() != nil {
		return Export{}, ctx.Err()
	}
	// Reject a running task without waiting behind its long-lived execution lock.
	// Re-read under the lock before committing the transfer.
	current, err := o.read(id)
	if err != nil {
		return Export{}, err
	}
	if current.Phase != phaseRetained && current.Phase != phaseExporting && current.Phase != phaseHomeOwned {
		return Export{}, errors.New("work: completed retained execution required")
	}
	unlock := o.jobs.Lock(id)
	defer unlock()
	r, err := o.read(id)
	if err != nil {
		return Export{}, err
	}
	if r.Phase != phaseRetained && r.Phase != phaseExporting && r.Phase != phaseHomeOwned || r.Stopped == nil || r.CancelOperation != "" {
		return Export{}, errors.New("work: completed retained execution required")
	}
	if r.Transfer != "" && r.Transfer != operation {
		return Export{}, errors.New("work: transfer operation conflicts")
	}
	if claim && (r.Transfer != operation || digest != r.Stopped.CheckpointDigest) {
		return Export{}, errors.New("work: claim binding differs")
	}
	if err = private(o.path(id, ".checkpoint"), false); err != nil {
		return Export{}, err
	}
	b, err := os.ReadFile(o.path(id, ".checkpoint"))
	if err != nil {
		return Export{}, err
	}
	e := Export{ID: id, Harness: r.Launch.Harness, Stopped: *r.Stopped, Checkpoint: b, OperationID: operation, Claimed: r.Phase == phaseHomeOwned}
	if err = ValidateExport(e, o.config.MaxCheckpointBytes); err != nil {
		return Export{}, err
	}
	r.Transfer = operation
	if claim {
		r.Phase = phaseHomeOwned
		e.Claimed = true
	} else if r.Phase != phaseHomeOwned {
		r.Phase = phaseExporting
	}
	if err = o.save(r); err != nil {
		return Export{}, err
	}
	return e, nil
}

func (o *Owner) Recover(ctx context.Context, id string) (View, error) {
	if ctx.Err() != nil {
		return View{}, ctx.Err()
	}
	unlock := o.jobs.Lock(id)
	defer unlock()
	r, e := o.read(id)
	if e != nil {
		return View{}, e
	}
	if r.Phase == phaseUnknown {
		if r.CancelOperation != "" {
			r.Phase = phaseCancelling
		} else {
			switch r.Progress {
			case phasePending, phasePreparing, phaseActive, phaseFinalizing:
				r.Phase = r.Progress
			default:
				return View{}, errors.New("work: recoverable stage unavailable")
			}
		}
		r.Problem = ""
		if e = o.save(r); e != nil {
			return View{}, e
		}
		o.dispatch(id)
	}
	return view(r), nil
}
func (o *Owner) Cancel(ctx context.Context, id, operation string) (View, error) {
	if !canonicalID(operation) {
		return View{}, errors.New("work: canonical cancellation UUID required")
	}
	r, e := o.read(id)
	if e != nil {
		return View{}, e
	}
	if r.Phase == phaseHomeOwned || r.Phase == phaseExporting || r.Phase == phaseRetained {
		return View{}, errors.New("work: task already retained or transferred")
	}
	if r.CancelOperation != "" && r.CancelOperation != operation {
		return View{}, errors.New("work: cancellation conflicts")
	}
	r.CancelOperation = operation
	r.Phase = phaseCancelling
	if e = o.save(r); e != nil {
		return View{}, e
	}
	o.mu.Lock()
	cancel := o.running[id]
	if cancel != nil {
		cancel()
	}
	o.mu.Unlock()
	unlock := o.jobs.Lock(id)
	defer unlock()
	r, e = o.read(id)
	if e != nil {
		return View{}, e
	}
	o.abort(ctx, r)
	return o.Info(ctx, id)
}
func (o *Owner) abort(ctx context.Context, r record) {
	i := session.Incarnation{}
	if r.Incarnation != nil {
		i = *r.Incarnation
	} else if observation, err := o.config.Backend.Inspect(ctx, r.Launch); err == nil {
		// A lost preparation reply can still have an exactly registered Run/Pod.
		// Inspection supplies its identity; it grants neither activation nor a refund.
		i = observation.Incarnation
		if i != (session.Incarnation{}) {
			r.Incarnation = &i
		}
	}
	obs, e := o.config.Backend.Abort(ctx, r.Launch, i)
	if e != nil && o.config.Log != nil {
		_, _ = fmt.Fprintf(o.config.Log, "Could not cancel work session %s: %v\n", r.Launch.SessionID, e)
	}
	r.Phase = phaseUnknown
	r.Problem = "Cancellation requires recovery"
	if e == nil && obs.State == session.PlacementStopped && obs.Stopped != nil && obs.Stopped.ActualStopped && obs.Stopped.Receipt != nil {
		r.Phase = phaseCancelled
		r.Problem = ""
		r.Stopped = obs.Stopped
		r.Stopped.Checkpoint = nil
	}
	_ = o.save(r)
}
func (o *Owner) Close() error {
	o.closeOnce.Do(func() {
		o.cancel()
		o.mu.Lock()
		for _, c := range o.running {
			c()
		}
		o.mu.Unlock()
		o.wg.Wait()
		o.closeError = errors.Join(syscall.Flock(int(o.lock.Fd()), syscall.LOCK_UN), o.lock.Close())
	})
	return o.closeError
}

func (c Config) validate() error {
	if !filepath.IsAbs(c.Directory) || c.Principal == "" || c.Harness != harness.NativeCodex || c.Placement == "" || len(c.InstructionDigest) != 64 || c.MaxDuration <= 0 || c.MaxDuration > 24*time.Hour || c.MaxCheckpointBytes <= 0 || c.MaxCheckpointBytes > 64<<20 || c.Backend == nil || len(c.Repositories) == 0 || len(c.Allowances) == 0 {
		return errors.New("work: fixed owner configuration required")
	}
	for name, r := range c.Repositories {
		if name == "" || strings.ContainsAny(name, "/\\\x00") || (r.Snapshot == "" && !strings.HasPrefix(r.URL, "https://github.com/")) || (r.Snapshot != "" && !filepath.IsAbs(r.Snapshot)) {
			return errors.New("work: approved repository name and source required")
		}
	}
	return nil
}
