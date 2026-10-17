package home

import (
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
	"time"

	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/work"
	"github.com/google/uuid"
)

const stageRetained = "retained"

// WorkSource is authenticated trusted-owner transport, configured by the home
// administrator. Artifact contents and private socket arguments cannot claim a
// session; activation requires the source's separately acknowledged ownership.
type WorkSource interface {
	BeginExport(context.Context, string, string) (work.Export, error)
	Claim(context.Context, string, string, string) (work.Export, error)
}

func (s *Service) SetWorkSources(sources map[string]WorkSource) {
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
}

type catalogOperation struct {
	Action      string `json:"action"`
	Execution   string `json:"execution"`
	Destination string `json:"destination,omitempty"`
}

func checkCatalogOperation(r localSession, operation string, intent catalogOperation) error {
	if !validID(operation) || operation == r.Operation {
		return errors.New("home: distinct product operation UUID required")
	}
	if previous, ok := r.Operations[operation]; ok && previous != intent {
		return errors.New("home: product operation belongs to another execution or intent")
	}
	return nil
}

func (s *Service) bindCatalogOperation(r *localSession, operation string, intent catalogOperation) error {
	if err := checkCatalogOperation(*r, operation, intent); err != nil {
		return err
	}
	if _, exists := r.Operations[operation]; !exists && len(r.Operations) >= 1024 {
		return errors.New("home: product operation retention limit reached")
	}
	if r.Operations == nil {
		r.Operations = map[string]catalogOperation{}
	}
	r.Operations[operation] = intent
	return s.saveCatalog(*r)
}

func (s *Service) bindProductOperation(product, operation, action, execution, destination string) error {
	if product == "" {
		return nil
	}
	r, err := s.readCatalog(product)
	if err != nil {
		return err
	}
	return s.bindCatalogOperation(&r, operation, catalogOperation{Action: action, Execution: execution, Destination: destination})
}

func (s *Service) catalogDir() (string, error) {
	dir := filepath.Join(s.config.Directory, "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return dir, private(dir, true)
}

func (s *Service) readCatalog(id string) (localSession, error) {
	var r localSession
	if !validID(id) {
		return r, errors.New("home: canonical product session UUID required")
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
	if r.Version != 1 || r.ID != id || r.Original.ID != id || !validID(r.Operation) ||
		r.Original.OperationID != r.Operation || r.Source == "" ||
		(r.Current != "" && !validID(r.Current)) || len(r.Executions) > 256 {
		return r, errors.New("home: invalid retained session binding")
	}
	if r.Original.Project != "" && !project.ValidID(r.Original.Project) {
		return r, errors.New("home: invalid retained project identity")
	}
	if err = project.ValidateReference(r.Original.RepositoryReference); err != nil {
		return r, err
	}
	for _, execution := range r.Executions {
		if !validID(execution) {
			return r, errors.New("home: invalid retained execution reference")
		}
	}
	if len(r.Operations) > 1024 {
		return r, errors.New("home: invalid retained product operations")
	}
	for operation, intent := range r.Operations {
		if !validID(operation) || (intent.Execution != "" || intent.Action != ActionStop) && !slices.Contains(r.Executions, intent.Execution) ||
			(intent.Action != ActionResume && intent.Action != ActionStop && intent.Action != ActionMove && intent.Action != ActionCancel) {
			return r, errors.New("home: invalid retained product operation binding")
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

func (s *Service) validateExport(e work.Export, id, operation string) error {
	if err := validateExportAssociation(e); err != nil {
		return err
	}

	stopped := e.Stopped
	if e.ID != id || e.OperationID != operation || e.Harness != s.config.Local.Harness ||
		stopped.Launch.Harness != e.Harness || !validID(stopped.Launch.ThreadID) ||
		stopped.Launch.InstructionDigest != s.config.Local.InstructionDigest ||
		!stopped.ActualStopped || stopped.Receipt == nil ||
		!reflect.DeepEqual(stopped.Receipt.Launch, stopped.Launch) ||
		stopped.Receipt.Incarnation != stopped.Incarnation || stopped.Incarnation.ResourceUID == "" {
		return errors.New("home: exported writer stop, accounting or native binding is unavailable")
	}
	if len(stopped.Launch.Allowances) == 0 || len(stopped.Receipt.Usage) != len(stopped.Launch.Allowances) {
		return errors.New("home: exported accounting is incomplete")
	}
	for name, allowance := range stopped.Launch.Allowances {
		usage, ok := stopped.Receipt.Usage[name]
		if !ok || usage.Units < 0 || usage.CostMicros < 0 ||
			(allowance.Units.Bounded && usage.Units > allowance.Units.Value) ||
			(allowance.CostMicros.Bounded && usage.CostMicros > allowance.CostMicros.Value) {
			return errors.New("home: exported accounting is invalid")
		}
	}
	meta, err := checkpoint.Inspect(e.Checkpoint, s.config.Local.MaxCheckpointBytes)
	if err != nil {
		return err
	}
	if meta.SHA256 != stopped.CheckpointDigest || meta.Harness != e.Harness || meta.ThreadID != stopped.Launch.ThreadID {
		return errors.New("home: exported checkpoint conflicts with stopped source")
	}
	if e.CheckpointInfo != nil && !reflect.DeepEqual(*e.CheckpointInfo, meta) {
		return errors.New("home: exported checkpoint metadata differs from its artifact")
	}
	return checkpoint.ValidateContinuity(e.Checkpoint, e.Harness, s.config.Local.InstructionDigest, s.config.Local.MaxCheckpointBytes)
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
	if !validID(operation) || s.workSources[sourceName] == nil {
		return Status{}, errors.New("home: configured source and operation UUID required")
	}
	r, err := s.readCatalog(id)
	if err == nil {
		if r.Source != sourceName || r.Operation != operation {
			return Status{}, errors.New("home: pull intent conflicts")
		}
		if r.Claimed {
			return s.catalogStatus(r)
		}
	} else if !os.IsNotExist(err) {
		return Status{}, err
	} else {
		e, exportErr := s.workSources[sourceName].BeginExport(ctx, id, operation)
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
	e, err := s.workSources[sourceName].Claim(ctx, id, operation, r.Original.Stopped.CheckpointDigest)
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
func (s *Service) Resume(ctx context.Context, id, operation string) (Status, error) {
	unlock, err := s.lockSession(id)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	if !validID(operation) {
		return Status{}, errors.New("home: resume operation UUID required")
	}
	r, err := s.readCatalog(id)
	if err != nil {
		return Status{}, err
	}
	if !r.Claimed {
		return Status{}, errors.New("home: imported session ownership is not acknowledged")
	}
	allowances, err := s.projectAllowances(r)
	if err != nil {
		return Status{}, err
	}
	if previous, exists := r.Operations[operation]; operation == r.Operation || exists && previous.Action != ActionResume {
		return Status{}, errors.New("home: resume operation intent conflicts")
	}
	for _, execution := range r.Executions {
		prior, inspectErr := s.coordinator.Inspect(execution)
		if inspectErr != nil {
			return Status{}, inspectErr
		}
		if prior.Operations[0].ID == operation {
			if r.Current != execution {
				return Status{}, errors.New("home: resume operation belongs to an earlier execution")
			}
			if _, err = s.startFirstVisit(ctx, prior); err != nil {
				return Status{}, err
			}
			return s.catalogStatus(r)
		}
	}
	if r.Current != "" {
		return Status{}, errors.New("home: previous execution must stop and commit before resume")
	}
	if len(r.Executions) >= 256 {
		return Status{}, errors.New("home: execution retention limit reached")
	}
	data, err := s.catalogArtifact(r.Committed.CheckpointDigest)
	if err != nil {
		return Status{}, err
	}
	if err = checkpoint.ValidateContinuity(data, s.config.Local.Harness, s.config.Local.InstructionDigest, s.config.Local.MaxCheckpointBytes); err != nil {
		return Status{}, err
	}
	first := session.Launch{Harness: s.config.Local.Harness, SessionID: uuid.NewString(), OperationID: operation,
		AllocationID: uuid.NewString(), Epoch: 1, Placement: s.config.Local.Name,
		ThreadID: r.Committed.Launch.ThreadID, InstructionDigest: s.config.Local.InstructionDigest,
		IncludeIgnored: slices.Clone(r.Committed.Launch.IncludeIgnored), Deadline: time.Now().Add(s.duration).UTC(),
		Allowances: allowances, CheckpointDigest: r.Committed.CheckpointDigest}
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

func (s *Service) catalogStatus(r localSession) (Status, error) {
	var info *checkpoint.Metadata
	if r.Committed.CheckpointDigest != "" {
		data, err := s.catalogArtifact(r.Committed.CheckpointDigest)
		if err != nil {
			return Status{}, err
		}
		meta, err := checkpoint.Inspect(data, s.config.Local.MaxCheckpointBytes)
		if err != nil {
			return Status{}, err
		}
		info = &meta
	}
	if r.Current != "" {
		execution, err := s.coordinator.Inspect(r.Current)
		if err != nil {
			return Status{}, err
		}
		status := executionStatus(execution)
		status.Association, status.CheckpointInfo = r.Original.Association, info
		return status, nil
	}
	stage := "importing"
	ownership := "source"
	if r.Claimed {
		stage = stageRetained
		ownership = "home"
	}
	return Status{Association: r.Original.Association, CheckpointInfo: info,
		ID: r.ID, Ownership: ownership, ExecutionID: r.Committed.Launch.SessionID,
		AllocationID: r.Committed.Launch.AllocationID, Incarnation: &r.Committed.Incarnation,
		Placement: r.Committed.Launch.Placement, Phase: stage, Harness: r.Committed.Launch.Harness,
		InitialStage: stage, ThreadID: r.Committed.Launch.ThreadID, Deadline: r.Committed.Launch.Deadline}, nil
}

// resolveSession maps a session or execution ID to the execution to act on
// and, when catalogued, its durable session. An execution belonging to a
// catalogued session must still be that session's current writer.
func (s *Service) resolveSession(id string) (string, string, error) {
	r, err := s.readCatalog(id)
	if os.IsNotExist(err) {
		execution, inspectErr := s.coordinator.Inspect(id)
		if inspectErr == nil && execution.Session != "" {
			owner, ownerErr := s.readCatalog(execution.Session)
			if ownerErr != nil {
				return "", "", ownerErr
			}
			if owner.Current != id {
				return "", "", errors.New("home: execution is no longer the current session writer")
			}
			return id, execution.Session, nil
		}
		if inspectErr != nil && !os.IsNotExist(inspectErr) {
			return "", "", inspectErr
		}
		return id, "", nil
	}
	if err != nil {
		return "", "", err
	}
	if r.Current == "" {
		return "", id, errors.New("home: session has no active execution; use resume")
	}
	return r.Current, id, nil
}

func (s *Service) lockProductExecution(id string) (string, string, func(), error) {
	execution, product, err := s.resolveSession(id)
	if err != nil {
		return "", "", nil, err
	}
	if product == "" {
		return execution, product, func() {}, nil
	}
	unlock, err := s.lockSession(product)
	if err != nil {
		return "", "", nil, err
	}
	current, _, err := s.resolveSession(id)
	if err != nil || current != execution {
		unlock()
		if err == nil {
			err = errors.New("home: session writer changed")
		}
		return "", "", nil, err
	}
	return execution, product, unlock, nil
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
	execution, product, unlock, err := s.lockProductExecution(id)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	if err = s.bindProductOperation(product, operation, ActionMove, execution, destination); err != nil {
		return Status{}, err
	}
	status, err := s.moveExecution(ctx, execution, operation, destination)
	if product != "" {
		status.ID = product
	}
	return status, err
}

func (s *Service) Recover(ctx context.Context, id string) (Status, error) {
	execution, product, unlock, err := s.lockProductExecution(id)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	status, err := s.recoverExecution(ctx, execution)
	if product != "" {
		status.ID = product
	}
	return status, err
}

func (s *Service) Cancel(ctx context.Context, id, operation string) (Status, error) {
	execution, product, err := s.resolveSession(id)
	if err != nil {
		return Status{}, err
	}
	if !validID(operation) {
		return Status{}, errors.New("home: cancellation operation UUID required")
	}
	if product != "" {
		r, readErr := s.readCatalog(product)
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
		if product == "" {
			return func() {}, nil
		}
		unlock, lockErr := s.lockSession(product)
		if lockErr != nil {
			return nil, lockErr
		}
		current, _, resolveErr := s.resolveSession(id)
		if resolveErr != nil || current != execution {
			unlock()
			return nil, errors.New("home: cancellation target is no longer the current writer")
		}
		if bindErr := s.bindProductOperation(product, operation, ActionCancel, execution, ""); bindErr != nil {
			unlock()
			return nil, bindErr
		}
		return unlock, nil
	}
	r, err := s.coordinator.Cancel(ctx, execution, operation, admit)
	return executionStatus(r), err
}

func (s *Service) Attach(ctx context.Context, id string, input io.Reader, output io.Writer) error {
	execution, _, err := s.resolveSession(id)
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

func validateExportAssociation(e work.Export) error {
	if e.Project != "" && !project.ValidID(e.Project) {
		return errors.New("home: invalid exported project identity")
	}
	return project.ValidateReference(e.RepositoryReference)
}
