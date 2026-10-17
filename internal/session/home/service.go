package home

import (
	"cmp"
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	budgetfile "github.com/andrewmccall/sproozi/internal/budget/file"
	"github.com/andrewmccall/sproozi/internal/checkpoint"
	gatewaydocker "github.com/andrewmccall/sproozi/internal/gateway/docker"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/workspace"
	"github.com/google/uuid"
)

// maxRetainedExecutions bounds the executions the home coordinator retains.
const maxRetainedExecutions = 256

type localPlacement interface {
	session.Placement
	session.TerminalPlacement
	session.PreparationAborter
}

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
	local       localPlacement
	placements  map[string]session.Placement
	coordinator *session.Coordinator
	owner       *gatewaydocker.Owner
	ledger      *budgetfile.Ledger
	remotes     []*profile.Client
	gateway     *http.Server
	closeOnce   sync.Once
	closeError  error
	workSources map[string]WorkSource
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

func validID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u != uuid.Nil && u.String() == id
}

// lockSession serializes one catalogued durable session.
func (s *Service) lockSession(id string) (func(), error) {
	if !validID(id) {
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
	temp, err := os.CreateTemp(directory, ".pending-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	if _, err = temp.Write(data); err == nil {
		err = temp.Sync()
	}
	err = errors.Join(err, temp.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(temp.Name(), filepath.Join(directory, name)); err != nil {
		return err
	}
	parent, err := os.Open(directory)
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
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
func (s *Service) Start(ctx context.Context, id, operationID, path string, includeIgnored []string) (Status, error) {
	if err := workspace.ValidateIgnoredPaths(includeIgnored); err != nil {
		return Status{}, err
	}
	if !validID(id) || !validID(operationID) || !filepath.IsAbs(path) {
		return Status{}, errors.New("home: UUIDs and absolute stopped workspace required")
	}
	r, err := s.coordinator.Inspect(id)
	if os.IsNotExist(err) {
		r, err = s.createWorkspaceExecution(ctx, id, operationID, path, includeIgnored)
	}
	if err != nil {
		return Status{}, err
	}
	first := r.Operations[0].Launch
	if r.Session != "" || first.CheckpointDigest != "" || first.OperationID != operationID || !slices.Equal(first.IncludeIgnored, includeIgnored) {
		return Status{}, errors.New("home: start intent conflicts with retained launch")
	}
	return s.startFirstVisit(ctx, r)
}

func (s *Service) createWorkspaceExecution(ctx context.Context, id, operationID, path string, includeIgnored []string) (session.Record, error) {
	first := session.Launch{Harness: s.config.Local.Harness, SessionID: id, OperationID: operationID,
		AllocationID: uuid.NewString(), Epoch: 1, Placement: s.config.Local.Name,
		InstructionDigest: s.config.Local.InstructionDigest, IncludeIgnored: slices.Clone(includeIgnored),
		Deadline: time.Now().Add(s.duration).UTC(), Allowances: maps.Clone(s.config.Allowances)}
	return s.createExecution(first, "", func() ([]byte, error) {
		return workspace.Capture(ctx, path, includeIgnored, s.config.Local.MaxCheckpointBytes)
	})
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

func (s *Service) moveExecution(ctx context.Context, id, operationID, destination string) (Status, error) {
	if !validID(operationID) || s.placements[destination] == nil {
		return Status{}, errors.New("home: canonical operation UUID and configured destination required")
	}
	r, err := s.coordinator.Inspect(id)
	if err != nil {
		return Status{}, err
	}
	if err = s.executionProjectApproval(r); err != nil {
		return Status{}, err
	}
	r, err = s.coordinator.Move(ctx, id, operationID, destination)
	return executionStatus(r), err
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

func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.closed = true
		if s.gateway != nil {
			s.closeError = errors.Join(s.closeError, s.gateway.Close())
		}
		if s.coordinator != nil {
			s.closeError = errors.Join(s.closeError, s.coordinator.Close())
		}
		if closer, ok := s.local.(io.Closer); ok {
			s.closeError = errors.Join(s.closeError, closer.Close())
		}
		for _, remote := range s.remotes {
			remote.Close()
		}
		for _, source := range s.workSources {
			if closer, ok := source.(io.Closer); ok {
				s.closeError = errors.Join(s.closeError, closer.Close())
			}
		}
		if s.owner != nil {
			s.closeError = errors.Join(s.closeError, s.owner.Close())
		}
		if s.ledger != nil {
			s.closeError = errors.Join(s.closeError, s.ledger.Close())
		}
	})
	return s.closeError
}
