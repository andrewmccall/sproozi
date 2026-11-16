package home

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/deadline"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/project"
	runtimedocker "github.com/andrewmccall/sproozi/internal/runtime/docker"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/work"
	"github.com/google/uuid"
)

// WorkSource is authenticated trusted-owner transport, configured by the home
// administrator. Artifact contents and private socket arguments cannot claim a
// session; activation requires the source's separately acknowledged ownership.
type WorkSource interface {
	BeginExport(context.Context, string, string) (work.Export, error)
	Claim(context.Context, string, string, string) (work.Export, error)
}

func (s *Service) SetWorkSources(sources map[string]WorkSource) {
	s.remoteMu.Lock()
	defer s.remoteMu.Unlock()
	s.workSources = maps.Clone(sources)
}

type localSession struct {
	Version    int                         `json:"version"`
	ID         string                      `json:"id"`
	Source     string                      `json:"source"`
	Operation  string                      `json:"operation"`
	Original   work.Export                 `json:"original"`
	Claimed    bool                        `json:"claimed"`
	Committed  session.Stopped             `json:"committed"`
	Current    string                      `json:"current,omitempty"`
	Executions []string                    `json:"executions,omitempty"`
	Operations map[string]catalogOperation `json:"operations,omitempty"`
	// MaxDuration is the session's own choice of deadline for each execution:
	// "none" or a Go duration. Empty follows the home's maxDuration (ADR 0056).
	MaxDuration string `json:"maxDuration,omitempty"`
}

type catalogOperation struct {
	Action      string `json:"action"`
	Execution   string `json:"execution"`
	Destination string `json:"destination,omitempty"`
}

func checkCatalogOperation(r localSession, operation string, intent catalogOperation) error {
	if !work.CanonicalID(operation) || operation == r.Operation {
		return errors.New("home: distinct session operation UUID required")
	}
	if previous, ok := r.Operations[operation]; ok && previous != intent {
		return errors.New("home: session operation belongs to another execution or intent")
	}
	return nil
}

func (s *Service) bindCatalogOperation(r *localSession, operation string, intent catalogOperation) error {
	if err := checkCatalogOperation(*r, operation, intent); err != nil {
		return err
	}
	if _, exists := r.Operations[operation]; !exists && len(r.Operations) >= 1024 {
		return errors.New("home: session operation retention limit reached")
	}
	if r.Operations == nil {
		r.Operations = map[string]catalogOperation{}
	}
	r.Operations[operation] = intent
	return s.saveCatalog(*r)
}

func (s *Service) bindSessionOperation(sessionID, operation, action, execution, destination string) error {
	if sessionID == "" {
		return nil
	}
	r, err := s.readCatalog(sessionID)
	if err != nil {
		return err
	}
	return s.bindCatalogOperation(&r, operation, catalogOperation{Action: action, Execution: execution, Destination: destination})
}

func (s *Service) catalogDir() (string, error) {
	dir := filepath.Join(s.config.Directory, "sessions")
	if err := privatefs.EnsureDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

func (s *Service) readCatalog(id string) (localSession, error) {
	var r localSession
	if !work.CanonicalID(id) {
		return r, errors.New("home: canonical session UUID required")
	}
	dir, err := s.catalogDir()
	if err != nil {
		return r, err
	}
	data, err := readPrivate(filepath.Join(dir, id+".json"), 1<<20)
	if err != nil {
		return r, err
	}
	if err = decode(data, &r); err != nil {
		return r, err
	}
	if r.Version != 1 || r.ID != id || r.Original.ID != id || !work.CanonicalID(r.Operation) ||
		r.Original.OperationID != r.Operation || r.Source == "" ||
		(r.Current != "" && !work.CanonicalID(r.Current)) || len(r.Executions) > 256 {
		return r, errors.New("home: invalid retained session binding")
	}
	if r.Original.Project != "" && !project.ValidID(r.Original.Project) {
		return r, errors.New("home: invalid retained project identity")
	}
	if err = project.ValidateReference(r.Original.RepositoryReference); err != nil {
		return r, err
	}
	for _, execution := range r.Executions {
		if !work.CanonicalID(execution) {
			return r, errors.New("home: invalid retained execution reference")
		}
	}
	if len(r.Operations) > 1024 {
		return r, errors.New("home: invalid retained session operations")
	}
	for operation, intent := range r.Operations {
		if !work.CanonicalID(operation) || (intent.Execution != "" || intent.Action != ActionStop) && !slices.Contains(r.Executions, intent.Execution) ||
			(intent.Action != ActionResume && intent.Action != ActionStop && intent.Action != ActionMove && intent.Action != ActionCancel) {
			return r, errors.New("home: invalid retained session operation binding")
		}
	}
	return r, nil
}

func (s *Service) saveCatalog(r localSession) error {
	dir, err := s.catalogDir()
	if err != nil {
		return err
	}
	data, err := encodeCatalog(r)
	if err != nil {
		return err
	}
	s.storageMu.Lock()
	defer s.storageMu.Unlock()
	return s.writeCatalog(dir, r.ID+".json", data)
}

// Callers hold storageMu so independent imports cannot over-admit retained
// bytes. Replacement journals count only their new size; immutable artifacts
// and crash leftovers remain charged until the operator removes them.
func (s *Service) writeCatalog(dir, name string, data []byte) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	count := len(entries)
	bytes := int64(len(data))
	for _, entry := range entries {
		if entry.Name() == name {
			count--
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		bytes += info.Size()
	}
	if count >= 512 || bytes > s.config.Local.RetainedBytes {
		return errors.New("home: catalogue retention limit reached")
	}
	return atomicWrite(dir, name, data)
}

func encodeCatalog(r localSession) ([]byte, error) {
	// Native artifacts are separate bounded files; journals retain trusted source
	// facts and account identities, never a second embedded copy of the transcript.
	r.Original.Checkpoint = nil
	r.Original.Stopped.Checkpoint = nil
	r.Committed.Checkpoint = nil
	data, err := json.Marshal(r)
	if err == nil && len(data) > 1<<20 {
		return nil, errors.New("home: session journal size limit reached")
	}
	return data, err
}

func validDigest(digest string) bool {
	data, err := hex.DecodeString(digest)
	return err == nil && len(data) == sha256.Size && strings.ToLower(digest) == digest
}

func (s *Service) retainCatalogArtifact(data []byte, digest string) error {
	if fmt.Sprintf("%x", sha256.Sum256(data)) != digest || int64(len(data)) > s.config.Local.MaxCheckpointBytes {
		return errors.New("home: imported checkpoint digest or size is invalid")
	}
	dir, err := s.catalogDir()
	if err != nil {
		return err
	}
	s.storageMu.Lock()
	defer s.storageMu.Unlock()
	path := filepath.Join(dir, digest+".checkpoint")
	existing, err := readPrivate(path, s.config.Local.MaxCheckpointBytes)
	if err == nil {
		if !slices.Equal(existing, data) {
			return errors.New("home: retained checkpoint changed")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	return s.writeCatalog(dir, digest+".checkpoint", data)
}

func (s *Service) catalogArtifact(digest string) ([]byte, error) {
	if !validDigest(digest) {
		return nil, errors.New("home: checkpoint digest required")
	}
	dir, err := s.catalogDir()
	if err != nil {
		return nil, err
	}
	data, err := readPrivate(filepath.Join(dir, digest+".checkpoint"), s.config.Local.MaxCheckpointBytes)
	if err == nil && fmt.Sprintf("%x", sha256.Sum256(data)) != digest {
		return nil, errors.New("home: retained checkpoint changed")
	}
	return data, err
}

// validateExport checks the source's stop, accounting and checkpoint evidence,
// then that the export belongs to this request and this home's native binding.
func (s *Service) validateExport(e work.Export, id, operation string) error {
	if err := work.ValidateExport(e, s.config.Local.MaxCheckpointBytes); err != nil {
		return fmt.Errorf("home: exported stop, accounting or checkpoint is invalid: %w", err)
	}
	launch := e.Stopped.Launch
	local, err := s.localFor(e.Harness)
	if err != nil {
		return err
	}
	if e.ID != id || e.OperationID != operation || e.Harness != local.Harness ||
		!work.CanonicalID(launch.ThreadID) || launch.InstructionDigest != local.InstructionDigest ||
		e.Stopped.Incarnation.ResourceUID == "" || len(launch.Allowances) == 0 {
		return errors.New("home: export does not match this pull or this home's native binding")
	}
	return nil
}

// Pull persists downloaded continuity inertly, then obtains and retains an
// authenticated claim from the configured owner. Interrupted claims replay the
// same operation; missing replies do not make the imported bytes activatable.
func (s *Service) Pull(ctx context.Context, id, operation, sourceName string) (Status, error) {
	unlock, err := s.lockSession(id)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	if !work.CanonicalID(operation) {
		return Status{}, errors.New("home: pull operation UUID required")
	}
	r, err := s.readCatalog(id)
	switch {
	case err == nil && r.Claimed:
		return s.catalogStatus(r)
	case err == nil:
		// A retained pull continues with its own source and operation, so
		// repeating the command finishes it.
		sourceName, operation = r.Source, r.Operation
	case !os.IsNotExist(err):
		return Status{}, err
	}
	source := s.workSource(sourceName)
	if source == nil {
		if sourceName == "" {
			return Status{}, rejectedError(fmt.Sprintf("home: the Sproozi service does not know which cluster holds "+
				"session %s; pass --context", id))
		}
		return Status{}, rejectedError(fmt.Sprintf("home: no remote executor named %q is known to this service; "+
			"reach it with --context", sourceName))
	}
	if r.ID == "" {
		e, exportErr := source.BeginExport(ctx, id, operation)
		if exportErr != nil {
			return Status{}, exportErr
		}
		if err = s.validateExport(e, id, operation); err != nil {
			return Status{}, err
		}
		if err = s.retainCatalogArtifact(e.Checkpoint, e.Stopped.CheckpointDigest); err != nil {
			return Status{}, err
		}
		r = localSession{Version: 1, ID: id, Source: sourceName, Operation: operation, Original: e, Committed: e.Stopped}
		if err = s.saveCatalog(r); err != nil {
			return Status{}, err
		}
	}
	e, err := source.Claim(ctx, id, operation, r.Original.Stopped.CheckpointDigest)
	if err != nil {
		return Status{}, err
	}
	if err = s.validateExport(e, id, operation); err != nil {
		return Status{}, err
	}
	if !e.Claimed || e.ID != id || e.OperationID != operation || e.Harness != r.Original.Harness ||
		!sameExportSource(e, r.Original) {
		return Status{}, errors.New("home: source ownership acknowledgement conflicts")
	}
	r.Claimed = true
	r.Original.Claimed = true
	r.Original.Association, r.Original.CheckpointInfo = e.Association, e.CheckpointInfo
	if err = s.saveCatalog(r); err != nil {
		return Status{}, err
	}
	return s.catalogStatus(r)
}

func sameExportSource(a, b work.Export) bool {
	// Metadata is derived from the exact validated artifact and may be added by
	// an upgraded owner during interrupted claim replay. Old standalone exports
	// had no association; descriptive provenance may be added, but never a Project.
	a.CheckpointInfo, b.CheckpointInfo = nil, nil
	if b.Association == (project.Association{}) && a.Project == "" {
		b.Association = a.Association
	}
	a.Checkpoint, b.Checkpoint = nil, nil
	a.Stopped.Checkpoint, b.Stopped.Checkpoint = nil, nil
	a.Claimed, b.Claimed = false, false
	return reflect.DeepEqual(a, b)
}

// Resume admits a new execution from claimed continuity. Its new UUID,
// allocation, deadline and allowances never modify or credit the source receipt.
// harnessArgs are the user's own arguments for the stock harness, kept with
// the new execution only.
func (s *Service) Resume(ctx context.Context, id, operation string, harnessArgs []string) (Status, error) {
	return s.ResumeWithMaxDuration(ctx, id, operation, harnessArgs, "")
}

// ResumeWithMaxDuration is Resume with a new choice of deadline for the session:
// "none", a positive Go duration, or empty to keep the session's own choice,
// or the home's maxDuration when it made none. A choice is kept for later
// resumes.
func (s *Service) ResumeWithMaxDuration(ctx context.Context, id, operation string, harnessArgs []string, maxDuration string) (Status, error) {
	unlock, err := s.lockSession(id)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	if !work.CanonicalID(operation) {
		return Status{}, errors.New("home: resume operation UUID required")
	}
	r, err := s.readCatalog(id)
	if os.IsNotExist(err) {
		if execution, inspectErr := s.coordinator.Inspect(id); inspectErr == nil {
			// A start interrupted before its catalogue entry is adopted now.
			if execution.Session != "" || execution.Operations[0].Launch.CheckpointDigest != "" {
				return Status{}, notResumable(id, execution)
			}
			if err = s.catalogStartedLocked(execution, startedChoice(execution)); err == nil {
				r, err = s.readCatalog(id)
			}
		}
	}
	if err != nil {
		return Status{}, err
	}
	if err = s.reconcileForResume(ctx, &r); err != nil {
		return Status{}, err
	}
	allowances, err := s.projectAllowances(r)
	if err != nil {
		return Status{}, err
	}
	if previous, exists := r.Operations[operation]; operation == r.Operation || exists && previous.Action != ActionResume {
		return Status{}, errors.New("home: resume operation intent conflicts")
	}
	if status, replayed, err := s.replayResume(ctx, &r, operation); replayed || err != nil {
		return status, err
	}
	if r.Current != "" {
		if err = s.refuseIn(r, ActionResume); err != nil {
			return Status{}, err
		}
		// The table admits resume only of a stopped session or a late start,
		// which replayResume ends; one it could not end is still current.
		return Status{}, fmt.Errorf("home: session %s still has a current execution; run \"sproozi status %s\"", id, id)
	}
	if len(r.Executions) >= 256 {
		return Status{}, errors.New("home: execution retention limit reached")
	}
	local, err := s.localFor(r.Committed.Launch.Harness)
	if err != nil {
		return Status{}, err
	}
	if err = harness.ValidateNativeArgs(local.Harness, harnessArgs); err != nil {
		return Status{}, rejectedError("home: " + err.Error())
	}
	if maxDuration != "" {
		r.MaxDuration = maxDuration
	}
	until, err := s.executionDeadline(r.MaxDuration)
	if err != nil {
		return Status{}, err
	}
	data, err := s.resumeInput(r, local)
	if err != nil {
		return Status{}, err
	}
	first := session.Launch{Harness: local.Harness, ExecutionID: uuid.NewString(), OperationID: operation,
		AllocationID: uuid.NewString(), Epoch: 1, Placement: local.Name,
		ThreadID: cmp.Or(r.Committed.ThreadID, r.Committed.Launch.ThreadID), InstructionDigest: local.InstructionDigest,
		IncludeIgnored: slices.Clone(r.Committed.Launch.IncludeIgnored), HarnessArgs: slices.Clone(harnessArgs),
		Login:    r.Committed.Launch.Login,
		Deadline: until, Allowances: allowances, CheckpointDigest: r.Committed.CheckpointDigest}
	if first.CheckpointDigest == "" {
		// Stopped with no conversation: start one again from its workspace.
		first.ThreadID = ""
	}
	execution, err := s.createExecution(first, id, func() ([]byte, error) { return data, nil })
	if err != nil {
		return Status{}, err
	}
	r.Current = execution.ID
	r.Executions = append(r.Executions, execution.ID)
	if err = s.bindCatalogOperation(&r, operation, catalogOperation{Action: ActionResume, Execution: execution.ID}); err != nil {
		return Status{}, err
	}
	if _, err = s.startFirstVisit(ctx, execution); err != nil {
		return Status{}, err
	}
	return s.catalogStatus(r)
}

// reconcileForResume refuses a resume the session's state refuses before
// anything is released or bound, when the session has no current execution,
// since nothing resume does next changes it; then it retires an idle current
// execution and releases the executions the session never recorded. The
// resume does not need those gone: a failure is logged, and the next command
// tries again. The caller holds the session's lock.
func (s *Service) reconcileForResume(ctx context.Context, r *localSession) error {
	if r.Current == "" {
		if err := s.refuseIn(*r, ActionResume); err != nil {
			return err
		}
	}
	if err := s.retireSettled(r); err != nil {
		return err
	}
	if err := s.releaseOrphans(ctx, *r); err != nil {
		s.logf("Failed to release an execution session %s never recorded: %v\n", r.ID, err)
	}
	return nil
}

// replayResume continues a resume operation the session already bound to an
// execution, reporting replayed. Otherwise it ends the current start or move
// if its deadline passed (ADR 0060), so a new resume continues the session
// instead of being refused for it. The caller holds the session's lock.
func (s *Service) replayResume(ctx context.Context, r *localSession, operation string) (Status, bool, error) {
	for _, execution := range r.Executions {
		prior, err := s.coordinator.Inspect(execution)
		if err != nil {
			return Status{}, false, err
		}
		if prior.Operations[0].ID != operation {
			continue
		}
		if r.Current != execution && !prior.Abandoned(operation) {
			return Status{}, true, errors.New("home: resume operation belongs to an earlier execution")
		}
		if _, err = s.startFirstVisit(ctx, prior); errors.Is(err, session.ErrDeadlinePassed) {
			// The deadline ended this resume before it started; a new
			// resume continues the session from what it kept.
			return Status{}, true, errors.Join(err, s.retireSettled(r))
		} else if err != nil {
			return Status{}, true, err
		}
		status, err := s.catalogStatus(*r)
		return status, true, err
	}
	if r.Current == "" {
		return Status{}, false, nil
	}
	return Status{}, false, s.endLate(ctx, r)
}

// startedChoice recovers the deadline choice of a start interrupted before
// its catalogue entry: a first visit without a deadline asked for none; one
// with a deadline is left to the home's maxDuration.
func startedChoice(execution session.Record) string {
	if !deadline.At(execution.Deadline).Set() {
		return deadline.NoneArg
	}
	return ""
}

// notResumable explains a resume addressed to an execution rather than a
// session.
func notResumable(id string, execution session.Record) error {
	if execution.Session != "" {
		return rejectedError(fmt.Sprintf("home: %s is an execution of session %s; resume the session UUID %s",
			id, execution.Session, execution.Session))
	}
	return rejectedError(fmt.Sprintf("home: %s is an execution, not a session; run \"sproozi ls\" to find "+
		"its session", id))
}

// resumeInput is what a resumed execution restores: the session's last
// checkpoint, or, for a session stopped with no conversation, the workspace
// its last execution started from.
func (s *Service) resumeInput(r localSession, local runtimedocker.PlacementConfig) ([]byte, error) {
	if r.Committed.CheckpointDigest == "" {
		if r.Committed.Launch.ExecutionID == "" {
			return nil, errors.New("home: session has no stopped execution to resume")
		}
		return s.coordinator.Workspace(r.Committed.Launch.ExecutionID)
	}
	data, err := s.catalogArtifact(r.Committed.CheckpointDigest)
	if err != nil {
		return nil, err
	}
	return data, checkpoint.ValidateContinuity(data, local.Harness, local.InstructionDigest, local.MaxCheckpointBytes)
}

// checkpointInfo describes a stop's checkpoint: the bytes it carries, or the
// catalogue's retained artifact. It is nil for a stop with no conversation.
func (s *Service) checkpointInfo(stopped session.Stopped) (*checkpoint.Metadata, error) {
	if stopped.CheckpointDigest == "" {
		return nil, nil
	}
	data := stopped.Checkpoint
	if data == nil {
		var err error
		if data, err = s.catalogArtifact(stopped.CheckpointDigest); err != nil {
			return nil, err
		}
	}
	meta, err := checkpoint.Inspect(data, s.config.Local.MaxCheckpointBytes)
	if err != nil {
		return nil, err
	}
	return &meta, nil
}

// catalogStatus reports a session in its derived phase. A session whose
// current execution is idle reads as stopped with that execution's stop, as
// it will once retired; reading changes nothing.
func (s *Service) catalogStatus(r localSession) (Status, error) {
	st, current, err := s.sessionState(r)
	if err != nil {
		return Status{}, err
	}
	committed := r.Committed
	if current != nil && st.stopped() {
		if committed, err = s.settledStop(r, *current); err != nil {
			return Status{}, err
		}
	}
	info, err := s.checkpointInfo(committed)
	if err != nil {
		return Status{}, err
	}
	if current != nil && !st.stopped() {
		status := executionStatus(*current)
		status.Association, status.CheckpointInfo = r.Original.Association, info
		return status, nil
	}
	ownership := ownershipHome
	if st.phase == PhaseImporting {
		ownership = ownershipSource
	}
	return Status{Association: r.Original.Association, CheckpointInfo: info,
		ID: r.ID, Ownership: ownership, ExecutionID: committed.Launch.ExecutionID,
		AllocationID: committed.Launch.AllocationID, Incarnation: &committed.Incarnation,
		Placement: committed.Launch.Placement, Phase: string(st.phase), Harness: committed.Launch.Harness,
		InitialStage: string(st.phase), ThreadID: cmp.Or(committed.ThreadID, committed.Launch.ThreadID),
		Deadline: committed.Launch.Deadline, Login: committed.Launch.Login, Exit: committed.Exit}, nil
}

// resolveSession maps a session or execution ID to the execution to act on
// and, when catalogued, its durable session. action is the command, which
// the session's state may refuse (nextSteps), whichever ID names it. An
// execution belonging to a catalogued session must still be that session's
// current writer, except that cancel reaches one the session never recorded
// (see unrecordedProblem); it then acts on that execution alone.
func (s *Service) resolveSession(id, action string) (string, string, error) {
	r, err := s.readCatalog(id)
	if os.IsNotExist(err) {
		execution, inspectErr := s.coordinator.Inspect(id)
		if inspectErr != nil && !os.IsNotExist(inspectErr) {
			return "", "", inspectErr
		}
		if inspectErr != nil || execution.Session == "" {
			return id, "", nil
		}
		owner, ownerErr := s.readCatalog(execution.Session)
		recorded := ownerErr == nil && (owner.Current == id || slices.Contains(owner.Executions, id))
		switch {
		case !recorded && action == ActionCancel && (ownerErr == nil || os.IsNotExist(ownerErr)):
			return id, "", nil
		case ownerErr != nil:
			return "", "", ownerErr
		case owner.Current != id:
			return "", "", errors.New("home: execution is no longer the current session writer")
		}
		r, id = owner, execution.Session
	} else if err != nil {
		return "", "", err
	}
	st, _, err := s.sessionState(r)
	if err != nil {
		return "", "", err
	}
	if err = refusal(id, st, action); err != nil {
		return "", id, err
	}
	if r.Current == "" {
		// The table refuses every command that resolves a session to its
		// execution once the session has none.
		return "", id, fmt.Errorf("home: session %s has no current execution", id)
	}
	return r.Current, id, nil
}

// lockSessionExecution resolves id like resolveSession and, for a catalogued
// session, holds its lock while the execution is still its current writer.
func (s *Service) lockSessionExecution(id, action string) (string, string, func(), error) {
	execution, sessionID, err := s.resolveSession(id, action)
	if err != nil {
		return "", "", nil, err
	}
	if sessionID == "" {
		return execution, sessionID, func() {}, nil
	}
	unlock, err := s.lockSession(sessionID)
	if err != nil {
		return "", "", nil, err
	}
	current, _, err := s.resolveSession(id, action)
	if err != nil || current != execution {
		unlock()
		if err == nil {
			err = errors.New("home: session writer changed")
		}
		return "", "", nil, err
	}
	return execution, sessionID, unlock, nil
}

func (s *Service) Status(id string) (Status, error) {
	r, err := s.readCatalog(id)
	if err == nil {
		return s.catalogStatus(r)
	}
	if !os.IsNotExist(err) {
		return Status{}, err
	}
	execution, err := s.coordinator.Inspect(id)
	if err != nil {
		return Status{}, err
	}
	return executionStatus(execution), nil
}

func (s *Service) Move(ctx context.Context, id, operation, destination string) (Status, error) {
	// "local" names this machine whatever its placement is called; a move to
	// any local placement continues on the session's own harness.
	if destination == LocalAlias {
		destination = s.config.Local.Name
	}
	// Refuse an unconfigured destination before binding the operation to it.
	if err := s.moveDestinationProblem(destination); err != nil {
		return Status{}, err
	}
	execution, sessionID, unlock, err := s.lockSessionExecution(id, ActionMove)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	if err = s.bindSessionOperation(sessionID, operation, ActionMove, execution, destination); err != nil {
		return Status{}, err
	}
	status, err := s.moveExecution(ctx, execution, operation, destination)
	if errors.Is(err, session.ErrDeadlinePassed) {
		// The deadline ended the move after its source stopped: the session
		// is stopped with that stop's checkpoint (ADR 0060).
		return s.settledStatus(sessionID, status, err)
	}
	return status, err
}

// Recover finishes the current execution's pending operation. A stop it
// finishes, or a start or move its deadline ends, leaves the session stopped.
func (s *Service) Recover(ctx context.Context, id string) (Status, error) {
	execution, sessionID, unlock, err := s.lockSessionExecution(id, ActionRecover)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	status, err := s.recoverExecution(ctx, execution)
	if errors.Is(err, session.ErrDeadlinePassed) || err == nil && status.Phase == string(session.PhaseIdle) {
		return s.settledStatus(sessionID, status, err)
	}
	return status, err
}

// settledStatus retires the locked session's current execution if it is idle
// and reports the session's status with err. Without a session it reports
// the execution's status.
func (s *Service) settledStatus(sessionID string, status Status, err error) (Status, error) {
	if sessionID == "" {
		return status, err
	}
	r, readErr := s.readCatalog(sessionID)
	if readErr == nil {
		readErr = s.retireSettled(&r)
	}
	if readErr != nil {
		return status, errors.Join(err, readErr)
	}
	settled, statusErr := s.catalogStatus(r)
	if statusErr != nil {
		return status, errors.Join(err, statusErr)
	}
	return settled, err
}

func (s *Service) Cancel(ctx context.Context, id, operation string) (Status, error) {
	execution, sessionID, err := s.resolveSession(id, ActionCancel)
	if err != nil {
		return Status{}, err
	}
	if !work.CanonicalID(operation) {
		return Status{}, errors.New("home: cancellation operation UUID required")
	}
	if sessionID != "" {
		r, readErr := s.readCatalog(sessionID)
		if readErr != nil {
			return Status{}, readErr
		}
		if err = checkCatalogOperation(r, operation, catalogOperation{Action: ActionCancel, Execution: execution}); err != nil {
			return Status{}, err
		}
	}
	// The coordinator interrupts graceful work on the execution, including a
	// blocked resume, before admit waits on the durable session's lock.
	admit := func() (func(), error) {
		if sessionID == "" {
			return s.lockUnrecordedCancel(execution)
		}
		unlock, lockErr := s.lockSession(sessionID)
		if lockErr != nil {
			return nil, lockErr
		}
		current, _, resolveErr := s.resolveSession(id, ActionCancel)
		if resolveErr != nil || current != execution {
			unlock()
			return nil, errors.New("home: cancellation target is no longer the current writer")
		}
		if bindErr := s.bindSessionOperation(sessionID, operation, ActionCancel, execution, ""); bindErr != nil {
			unlock()
			return nil, bindErr
		}
		return unlock, nil
	}
	r, err := s.coordinator.Cancel(ctx, execution, operation, admit)
	return executionStatus(r), err
}

// lockUnrecordedCancel admits the cancellation of an execution no session
// records. For an execution of a session, it holds that session's lock and
// checks again under it that the session does not record the execution: a
// resume records its new execution under that lock, so a cancellation never
// takes one a resume is about to start. An execution with no session needs
// no lock.
func (s *Service) lockUnrecordedCancel(id string) (func(), error) {
	execution, err := s.coordinator.Inspect(id)
	if err != nil {
		return nil, err
	}
	if execution.Session == "" {
		return func() {}, nil
	}
	unlock, recorded, err := s.lockUnrecorded(execution)
	if err != nil {
		return nil, err
	}
	if recorded {
		// A resume recorded it meanwhile: a repeated cancel reaches it as
		// the session's own execution.
		unlock()
		return nil, fmt.Errorf("home: session %s now records execution %s; run the cancellation again",
			execution.Session, id)
	}
	return unlock, nil
}

func (s *Service) Attach(ctx context.Context, id string, input io.Reader, output io.Writer) error {
	execution, _, err := s.resolveSession(id, ActionAttach)
	if err != nil {
		return err
	}
	return s.attachExecution(ctx, execution, input, output)
}

// Sessions enumerates compact owner metadata rather than native transcripts.
func (s *Service) Sessions(q work.SessionQuery) ([]Status, error) {
	dir, err := s.catalogDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var statuses []Status
	internal := map[string]bool{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		r, readErr := s.readCatalog(strings.TrimSuffix(entry.Name(), ".json"))
		if readErr != nil {
			return nil, readErr
		}
		status, readErr := s.catalogStatus(r)
		if readErr != nil {
			return nil, readErr
		}
		// Discovery exposes compact facts; detailed execution journals use Status.
		status.Session = nil
		if q.Project == "" || r.Original.Project == q.Project {
			statuses = append(statuses, status)
		}
		for _, execution := range r.Executions {
			internal[execution] = true
		}
	}
	executions, err := s.coordinator.List()
	if err != nil {
		return nil, err
	}
	for _, id := range executions {
		if internal[id] {
			continue
		}
		execution, readErr := s.coordinator.Inspect(id)
		if readErr != nil {
			return nil, readErr
		}
		// An execution that a catalogued session never recorded (an interrupted
		// resume) is not a session of its own.
		if execution.Session != "" {
			continue
		}
		status := executionStatus(execution)
		// Discovery exposes compact facts; detailed execution journals use Status.
		status.Session = nil
		if q.Project == "" {
			statuses = append(statuses, status)
		}
	}
	return statuses, nil
}
