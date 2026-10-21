package home

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	budgetfile "github.com/andrewmccall/sproozi/internal/budget/file"
	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/project"
	runtimedocker "github.com/andrewmccall/sproozi/internal/runtime/docker"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/work"
	"github.com/andrewmccall/sproozi/internal/workspace"
	"github.com/google/uuid"
)

// maxRetainedExecutions bounds the executions the home coordinator retains.
const maxRetainedExecutions = 256

// Service is the home daemon. Every execution, including its first visit, is
// a coordinator record: the coordinator alone prepares, activates, stops,
// cancels and recovers writers. The service adds workspace capture, retention
// bounds, Project approval and the catalogue of durable sessions.
type Service struct {
	mu sync.RWMutex
	// sessions serializes each catalogued durable session. Executions are
	// serialized by the coordinator.
	sessions    session.JournalLocks
	retentionMu sync.Mutex
	storageMu   sync.Mutex
	closed      bool
	config      Config
	duration    time.Duration
	stacks      []*localStack
	certs       *certificates
	placements  map[string]session.Placement
	coordinator *session.Coordinator
	// drain ends and settles model calls before the ledger closes.
	drain  *model.Drain
	ledger *budgetfile.Ledger
	// sweepStop ends the periodic recovery of expired reservation leases.
	sweepStop chan struct{}
	// legacySwept records that one recovery of reservations from before
	// leases has succeeded; until then each sweep repeats it.
	legacySwept atomic.Bool
	remotes     []*profile.Client
	closeOnce   sync.Once
	closeError  error
	workSources map[string]WorkSource
	// log receives the daemon's operator messages; nil discards them.
	log io.Writer
	// images resolves Docker image IDs; CheckDocker uses it to reach the engine.
	images imageResolver
}

// imageResolver is the part of the Docker engine CheckDocker needs.
type imageResolver interface {
	ImageID(ctx context.Context, reference string) (string, error)
	NetworkExists(ctx context.Context, name string) (bool, error)
}

// Status names the durable session and separately reports its bounded execution.
// Session retains the execution journal schema and its original authority bindings.
type Status struct {
	project.Association
	CheckpointInfo *checkpoint.Metadata `json:"checkpointInfo,omitempty"`
	ID             string               `json:"id"`
	ExecutionID    string               `json:"executionId,omitempty"`
	AllocationID   string               `json:"allocationId,omitempty"`
	Incarnation    *session.Incarnation `json:"incarnation,omitempty"`
	Harness        harness.NativeKind   `json:"harness"`
	Ownership      string               `json:"ownership"`
	Phase          string               `json:"phase"`
	Placement      string               `json:"placement,omitempty"`
	// InitialStage repeats Phase for existing clients.
	InitialStage string          `json:"initialStage"`
	ThreadID     string          `json:"threadId,omitempty"`
	Deadline     time.Time       `json:"deadline"`
	Session      *session.Record `json:"session,omitempty"`
}

// executionStatus reports an execution and its current visit: the active one,
// otherwise the last stopped one, otherwise the first visit still starting.
func executionStatus(r session.Record) Status {
	if r.ID == "" {
		return Status{}
	}
	out := Status{ID: cmp.Or(r.Session, r.ID), ExecutionID: r.ID, Harness: r.Harness, Ownership: "home",
		Phase: string(r.Phase), InitialStage: string(r.Phase), ThreadID: r.ThreadID, Deadline: r.Deadline, Session: &r}
	var launch *session.Launch
	var incarnation *session.Incarnation
	switch {
	case r.Active != nil:
		launch, incarnation = &r.Active.Launch, &r.Active.Incarnation
	case len(r.Visits) > 0:
		last := &r.Visits[len(r.Visits)-1]
		launch, incarnation = &last.Launch, &last.Incarnation
	default:
		launch, incarnation = r.Operations[0].Launch, r.Operations[0].Incarnation
	}
	out.Placement, out.AllocationID = launch.Placement, launch.AllocationID
	if incarnation != nil {
		i := *incarnation
		out.Incarnation = &i
	}
	return out
}

// lockSession serializes one catalogued durable session.
func (s *Service) lockSession(id string) (func(), error) {
	if !work.CanonicalID(id) {
		return nil, errors.New("home: canonical session UUID required")
	}
	s.mu.RLock()
	unlock := s.sessions.Lock(id)
	if s.closed {
		unlock()
		s.mu.RUnlock()
		return nil, errors.New("home: service closed")
	}
	return func() { unlock(); s.mu.RUnlock() }, nil
}

func atomicWrite(directory, name string, data []byte) error {
	return privatefs.Replace(directory, name, data)
}

// createExecution retains a new execution within the configured retention
// bounds. input supplies the first visit's restored bytes and runs only once
// the bounds admit another execution.
func (s *Service) createExecution(first session.Launch, owner string, input func() ([]byte, error)) (session.Record, error) {
	s.retentionMu.Lock()
	defer s.retentionMu.Unlock()
	executions, size, err := s.coordinator.Retained()
	if err != nil {
		return session.Record{}, err
	}
	if executions >= maxRetainedExecutions {
		return session.Record{}, errors.New("home: execution retention count reached")
	}
	if size > s.config.Local.RetainedBytes-s.config.Local.MaxCheckpointBytes {
		return session.Record{}, errors.New("home: execution retention byte limit reached")
	}
	data, err := input()
	if err != nil {
		return session.Record{}, err
	}
	return s.coordinator.Create(first, data, owner)
}

// Start captures a stopped ordinary Git directory and starts a new execution
// whose first visit restores it in local Docker. The native UUID comes from the
// real process at its first stop; Start never fabricates one. A retry with the
// same operation and ignored-file selection continues the retained first visit.
//
// kind selects a configured local harness; empty selects the primary one. A
// session keeps its harness; another harness starts a separate session.
func (s *Service) Start(ctx context.Context, id, operationID, path string, kind harness.NativeKind, includeIgnored []string) (Status, error) {
	if err := workspace.ValidateIgnoredPaths(includeIgnored); err != nil {
		return Status{}, err
	}
	if kind == "" {
		kind = s.config.Local.Harness
	}
	local, err := s.localFor(kind)
	if err != nil {
		return Status{}, err
	}
	if !work.CanonicalID(id) || !work.CanonicalID(operationID) || !filepath.IsAbs(path) {
		return Status{}, errors.New("home: UUIDs and absolute stopped workspace required")
	}
	r, err := s.coordinator.Inspect(id)
	if os.IsNotExist(err) {
		r, err = s.createWorkspaceExecution(ctx, id, operationID, path, local, includeIgnored)
	}
	if err != nil {
		return Status{}, err
	}
	first := r.Operations[0].Launch
	if r.Session != "" || first.CheckpointDigest != "" {
		return Status{}, rejectedError(fmt.Sprintf("home: start intent conflicts with retained launch; %s was "+
			"not created by start, see \"sproozi status %s\"", id, id))
	}
	if first.Harness != kind {
		return Status{}, rejectedError(fmt.Sprintf("home: start intent conflicts with retained launch; %s was "+
			"started with harness %s: start a new session for %s", id, first.Harness, kind))
	}
	if first.OperationID != operationID || !slices.Equal(first.IncludeIgnored, includeIgnored) {
		// A new operation conflicts again; only the retained one continues it.
		return Status{}, rejectedError(fmt.Sprintf("home: start intent conflicts with retained launch; %s was "+
			"started with operation %s: to continue it, repeat the original start command with --operation %s, "+
			"or run \"sproozi cancel %s\" to revoke and force-stop its current execution", id, first.OperationID, first.OperationID, id))
	}
	return s.startFirstVisit(ctx, r)
}

func (s *Service) createWorkspaceExecution(ctx context.Context, id, operationID, path string, local runtimedocker.PlacementConfig, includeIgnored []string) (session.Record, error) {
	first := session.Launch{Harness: local.Harness, ExecutionID: id, OperationID: operationID,
		AllocationID: uuid.NewString(), Epoch: 1, Placement: local.Name,
		InstructionDigest: local.InstructionDigest, IncludeIgnored: slices.Clone(includeIgnored),
		Deadline: time.Now().Add(s.duration).UTC(), Allowances: maps.Clone(s.config.Allowances)}
	return s.createExecution(first, "", func() ([]byte, error) {
		limit := s.config.Local.MaxCheckpointBytes
		data, err := workspace.CaptureLocal(ctx, path, includeIgnored, limit)
		if err != nil && ctx.Err() == nil {
			// Nothing is recorded yet, so repeating the request cannot help.
			return nil, rejectedError(workspaceProblem(path, err, limit))
		}
		return data, err
	})
}

// workspaceProblem explains why start cannot capture a workspace and what
// would let it.
func workspaceProblem(path string, err error, limit int64) string {
	if _, statErr := os.Lstat(path); os.IsNotExist(statErr) {
		return fmt.Sprintf("workspace: %s does not exist; pass --workspace the top-level directory of a Git repository", path)
	}
	switch {
	case errors.Is(err, workspace.ErrNotRepository):
		return fmt.Sprintf("%v; %s must be the top-level directory of a Git repository, holding its .git directory "+
			"(linked worktrees and submodules are unsupported)", err, path)
	case errors.Is(err, workspace.ErrByteLimit):
		return fmt.Sprintf("%v; local.maxCheckpointBytes is %.1f MiB: raise it in home's config.json, up to %d MiB, "+
			"keeping local.retainedBytes at least twice it, then restart home", err, float64(limit)/(1<<20),
			MaxCheckpointBytes>>20)
	}
	return err.Error()
}

// startFirstVisit prepares and activates a retained first visit that has not
// started yet, after checking current Project approval.
func (s *Service) startFirstVisit(ctx context.Context, r session.Record) (Status, error) {
	if r.PendingOperation != r.Operations[0].ID || r.CancelOperation != "" {
		return executionStatus(r), nil
	}
	if err := s.executionProjectApproval(r); err != nil {
		return Status{}, err
	}
	r, err := s.coordinator.Recover(ctx, r.ID)
	if err != nil {
		return Status{}, err
	}
	return executionStatus(r), nil
}

// moveDestinationProblem rejects a destination that names no configured
// placement, listing the ones that do.
func (s *Service) moveDestinationProblem(destination string) error {
	if s.placements[destination] != nil {
		return nil
	}
	return rejectedError(fmt.Sprintf("home: move destination %q is not a configured placement; use one of: %s",
		destination, strings.Join(slices.Sorted(maps.Keys(s.placements)), ", ")))
}

func (s *Service) moveExecution(ctx context.Context, id, operationID, destination string) (Status, error) {
	if !work.CanonicalID(operationID) {
		return Status{}, fmt.Errorf("home: move operation %q is not a canonical UUID", operationID)
	}
	if err := s.moveDestinationProblem(destination); err != nil {
		return Status{}, err
	}
	r, err := s.coordinator.Inspect(id)
	if err != nil {
		return Status{}, err
	}
	destination = s.localDestination(r, destination)
	if err = s.executionProjectApproval(r); err != nil {
		return Status{}, err
	}
	r, err = s.coordinator.Move(ctx, id, operationID, destination)
	return executionStatus(r), err
}

// localDestination maps any local placement name to the local placement of
// the execution's own harness, so "move --to local" returns every session to
// home Docker. Remote names are unchanged.
func (s *Service) localDestination(r session.Record, destination string) string {
	if len(r.Operations) == 0 {
		return destination
	}
	for _, stack := range s.stacks {
		if stack.config.Name == destination {
			if local, err := s.localFor(r.Operations[0].Launch.Harness); err == nil {
				return local.Name
			}
		}
	}
	return destination
}

func (s *Service) recoverExecution(ctx context.Context, id string) (Status, error) {
	r, err := s.coordinator.Inspect(id)
	if err != nil {
		return Status{}, err
	}
	if recoveryActivates(r) {
		if err = s.executionProjectApproval(r); err != nil {
			return Status{}, err
		}
	}
	r, err = s.coordinator.Recover(ctx, id)
	if err != nil {
		return Status{}, err
	}
	return executionStatus(r), nil
}

func (s *Service) attachExecution(ctx context.Context, id string, input io.Reader, output io.Writer) error {
	r, err := s.coordinator.Inspect(id)
	if err != nil {
		return err
	}
	if r.Active == nil || r.Phase != session.PhaseActive {
		return errors.New("home: native attachment is unavailable")
	}
	terminal, ok := s.placements[r.Active.Launch.Placement].(session.TerminalPlacement)
	if !ok {
		return errors.New("home: native attachment is unavailable")
	}
	return terminal.Attach(ctx, r.Active.Launch, r.Active.Incarnation, input, output)
}

// modelDrainTimeout bounds how long Close waits for model calls to settle.
const modelDrainTimeout = 10 * time.Second

func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		// Settle model calls while the ledger is still open; a call left
		// unsettled is recovered from the ledger later.
		drainCtx, cancelDrain := context.WithTimeout(context.Background(), modelDrainTimeout)
		_ = s.drain.Shutdown(drainCtx)
		cancelDrain()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.closed = true
		for _, stack := range s.stacks {
			if stack.server != nil {
				s.closeError = errors.Join(s.closeError, stack.server.Close())
			}
		}
		if s.coordinator != nil {
			s.closeError = errors.Join(s.closeError, s.coordinator.Close())
		}
		for _, stack := range s.stacks {
			if stack.placement != nil {
				s.closeError = errors.Join(s.closeError, stack.placement.Close())
			}
		}
		for _, remote := range s.remotes {
			remote.Close()
		}
		for _, source := range s.workSources {
			if closer, ok := source.(io.Closer); ok {
				s.closeError = errors.Join(s.closeError, closer.Close())
			}
		}
		for _, stack := range s.stacks {
			if stack.owner != nil {
				s.closeError = errors.Join(s.closeError, stack.owner.Close())
			}
		}
		if s.sweepStop != nil {
			close(s.sweepStop)
		}
		if s.ledger != nil {
			s.closeError = errors.Join(s.closeError, s.ledger.Close())
		}
	})
	return s.closeError
}

// ledgerSweepInterval is how often home recovers expired reservation leases.
const ledgerSweepInterval = time.Minute

// sweepLedger settles expired reservation leases, which a daemon that
// stopped without settling its model calls leaves behind.
func (s *Service) sweepLedger() {
	legacy := !s.legacySwept.Load()
	if err := s.ledger.Sweep(time.Now(), legacy); err != nil {
		_, _ = fmt.Fprintln(s.log, "Failed to recover expired budget reservations:", err)
		return
	}
	if legacy {
		s.legacySwept.Store(true)
	}
}

func (s *Service) sweepLedgerPeriodically() {
	ticker := time.NewTicker(ledgerSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.sweepStop:
			return
		case <-ticker.C:
			s.sweepLedger()
		}
	}
}
