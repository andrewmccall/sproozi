package home

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	budgetfile "github.com/andrewmccall/sproozi/internal/budget/file"
	gatewaydocker "github.com/andrewmccall/sproozi/internal/gateway/docker"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/internal/lifecycle"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/workspace"
	"github.com/google/uuid"
)

const (
	initialPreparing  = "preparing"
	initialActive     = "active"
	initialImporting  = "importing"
	initialImported   = "imported"
	initialCancelling = "cancelling"
	initialCancelled  = "cancelled"
	initialStopped    = "stopped"
)

type localPlacement interface {
	session.Placement
	session.TerminalPlacement
	CreateLocal(context.Context, session.Launch, []byte) (session.Incarnation, error)
	CancelInitial(context.Context, session.Launch) error
}

type Service struct {
	mu          sync.RWMutex
	journals    session.JournalLocks
	retentionMu sync.Mutex
	storageMu   sync.Mutex
	closed      bool
	graceful    lifecycle.Control
	config      Config
	duration    time.Duration
	initialDir  string
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

type initial struct {
	Version        int                  `json:"version"`
	Workspace      string               `json:"workspace"`
	ArtifactDigest string               `json:"artifactDigest"`
	Launch         session.Launch       `json:"launch"`
	Incarnation    *session.Incarnation `json:"incarnation,omitempty"`
	Stage          string               `json:"stage"`
	OperationID    string               `json:"operationId,omitempty"`
	Destination    string               `json:"destination,omitempty"`
	Input          string               `json:"input,omitempty"`
	Stopped        *session.Stopped     `json:"stopped,omitempty"`
	ProductID      string               `json:"productId,omitempty"`
}

// Status is a trusted journal snapshot. It contains no native history or token.
type Status struct {
	ID           string          `json:"id"`
	ProductID    string          `json:"productId,omitempty"`
	InitialStage string          `json:"initialStage"`
	ThreadID     string          `json:"threadId,omitempty"`
	Deadline     time.Time       `json:"deadline"`
	Session      *session.Record `json:"session,omitempty"`
}

func validID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u != uuid.Nil && u.String() == id
}

func (s *Service) lockSession(id string) (func(), error) {
	if !validID(id) {
		return nil, errors.New("home: canonical session UUID required")
	}
	s.mu.RLock()
	unlock := s.journals.Lock(id)
	if s.closed {
		unlock()
		s.mu.RUnlock()
		return nil, errors.New("home: service closed")
	}
	return func() { unlock(); s.mu.RUnlock() }, nil
}

func (s *Service) readInitial(id string) (initial, error) {
	var r initial
	if !validID(id) {
		return r, errors.New("home: canonical session UUID required")
	}
	data, err := readPrivate(filepath.Join(s.initialDir, id+".json"), 128<<10)
	if err != nil {
		return r, err
	}
	if err = decode(data, &r); err != nil {
		return r, err
	}
	if err = workspace.ValidateIgnoredPaths(r.Launch.IncludeIgnored); err != nil {
		return r, err
	}
	if r.Version != 1 || r.Launch.Harness != s.config.Local.Harness || r.Launch.SessionID != id || r.Launch.Placement != s.config.Local.Name || r.Launch.InstructionDigest != s.config.Local.InstructionDigest || !validID(r.Launch.OperationID) || !validID(r.Launch.AllocationID) || r.Launch.Epoch != 1 || r.Launch.Deadline.IsZero() || r.ArtifactDigest == "" {
		return r, errors.New("home: initial binding is unavailable")
	}
	if (r.Input == "" && r.Launch.CheckpointDigest != "") ||
		(r.Input == checkpointInput && (r.Launch.CheckpointDigest != r.ArtifactDigest || !validID(r.Launch.ThreadID))) ||
		(r.Input != "" && r.Input != checkpointInput) {
		return r, errors.New("home: initial input binding is unavailable")
	}
	if r.ProductID != "" && (!validID(r.ProductID) || r.Input != checkpointInput) {
		return r, errors.New("home: invalid execution owner binding")
	}
	switch r.Stage {
	case initialPreparing, initialActive, initialImporting, initialImported, initialCancelling, initialCancelled, initialStopped:
	default:
		return r, errors.New("home: invalid initial stage")
	}
	return r, nil
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
func (s *Service) saveInitial(r initial) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(data) > 128<<10 {
		return errors.New("home: initial record exceeds bound")
	}
	return s.writeInitial(r.Launch.SessionID+".json", data)
}

func (s *Service) writeInitial(name string, data []byte) error {
	s.storageMu.Lock()
	defer s.storageMu.Unlock()
	return atomicWrite(s.initialDir, name, data)
}

func (s *Service) status(r initial) (Status, error) {
	out := Status{ID: r.Launch.SessionID, InitialStage: r.Stage, ThreadID: r.Launch.ThreadID, Deadline: r.Launch.Deadline}
	if r.Stage == initialImported {
		record, err := s.coordinator.Inspect(out.ID)
		if err != nil {
			return out, err
		}
		out.Session = &record
		out.ThreadID = record.ThreadID
	}
	return out, nil
}
func (s *Service) executionStatus(id string) (Status, error) {
	unlock, err := s.lockSession(id)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	r, err := s.readInitial(id)
	if err != nil {
		return Status{}, err
	}
	return s.status(r)
}

// Start receives a stopped ordinary Git directory, retains one workspace input,
// then launches a fresh controlled native process. It never fabricates its UUID.
func (s *Service) Start(ctx context.Context, id, operationID, path string, includeIgnored []string) (Status, error) {
	unlock, err := s.lockSession(id)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	if err := workspace.ValidateIgnoredPaths(includeIgnored); err != nil {
		return Status{}, err
	}
	if !validID(id) || !validID(operationID) || !filepath.IsAbs(path) {
		return Status{}, errors.New("home: UUIDs and absolute stopped workspace required")
	}
	r, err := s.readInitial(id)
	missing := os.IsNotExist(err)
	if missing || err == nil && r.Stage == initialPreparing {
		var finish func()
		ctx, finish, err = s.graceful.Begin(ctx, id)
		if err != nil {
			return Status{}, err
		}
		defer finish()
	}
	if missing {
		r, err = s.captureInitial(ctx, id, operationID, path, includeIgnored)
		if err != nil {
			return Status{}, err
		}
	} else if err != nil {
		return Status{}, err
	}
	if r.Workspace != path || r.Launch.OperationID != operationID || !slices.Equal(r.Launch.IncludeIgnored, includeIgnored) {
		return Status{}, errors.New("home: start intent conflicts with retained launch")
	}
	if r.Stage == initialPreparing {
		r, err = s.start(ctx, r)
		if err != nil {
			return Status{}, err
		}
	}
	return s.status(r)
}

func (s *Service) captureInitial(ctx context.Context, id, operationID, path string, includeIgnored []string) (initial, error) {
	s.retentionMu.Lock()
	defer s.retentionMu.Unlock()
	if err := ctx.Err(); err != nil {
		return initial{}, err
	}
	if err := s.retention(); err != nil {
		return initial{}, err
	}
	data, err := workspace.Capture(ctx, path, includeIgnored, s.config.Local.MaxCheckpointBytes)
	if err != nil {
		return initial{}, err
	}
	if err = s.writeInitial(id+".workspace", data); err != nil {
		return initial{}, err
	}
	r := initial{Version: 1, Workspace: path, ArtifactDigest: fmt.Sprintf("%x", sha256.Sum256(data)), Launch: session.Launch{Harness: s.config.Local.Harness, SessionID: id, OperationID: operationID, AllocationID: uuid.NewString(), Epoch: 1, Placement: s.config.Local.Name, InstructionDigest: s.config.Local.InstructionDigest, IncludeIgnored: append([]string(nil), includeIgnored...), Deadline: time.Now().Add(s.duration).UTC(), Allowances: maps.Clone(s.config.Allowances)}, Stage: initialPreparing}
	return r, s.saveInitial(r)
}

func (s *Service) retention() error {
	s.storageMu.Lock()
	defer s.storageMu.Unlock()
	entries, err := os.ReadDir(s.initialDir)
	if err != nil {
		return err
	}
	var size int64
	if len(entries) > 256 {
		return errors.New("home: initial retention count reached")
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return err
		}
		size += info.Size()
	}
	if size > s.config.Local.RetainedBytes-s.config.Local.MaxCheckpointBytes {
		return errors.New("home: initial retention byte limit reached")
	}
	return nil
}

func (s *Service) start(ctx context.Context, r initial) (initial, error) {
	observation, err := s.local.Inspect(ctx, r.Launch)
	if err != nil {
		return r, err
	}
	switch observation.State {
	case session.PlacementAbsent, session.PlacementPreparing:
		if err = ctx.Err(); err != nil {
			return r, err
		}
		data, readErr := readPrivate(filepath.Join(s.initialDir, r.Launch.SessionID+".workspace"), s.config.Local.MaxCheckpointBytes)
		if readErr != nil {
			return r, readErr
		}
		if fmt.Sprintf("%x", sha256.Sum256(data)) != r.ArtifactDigest {
			return r, errors.New("home: initial workspace changed")
		}
		var instance session.Incarnation
		var prepareErr error
		if r.Input == checkpointInput {
			instance, prepareErr = s.local.Prepare(ctx, r.Launch, data)
		} else {
			instance, prepareErr = s.local.CreateLocal(ctx, r.Launch, data)
		}
		if prepareErr != nil {
			return r, prepareErr
		}
		r.Incarnation = &instance
	case session.PlacementPrepared, session.PlacementActive:
		if r.Incarnation != nil && *r.Incarnation != observation.Incarnation {
			return r, errors.New("home: initial incarnation changed")
		}
		r.Incarnation = &observation.Incarnation
	default:
		return r, errors.New("home: initial preparation remains unknown")
	}
	if err = s.saveInitial(r); err != nil {
		return r, err
	}
	if observation.State != session.PlacementActive {
		if err = ctx.Err(); err != nil {
			return r, err
		}
		if err = s.local.Activate(ctx, r.Launch, *r.Incarnation); err != nil {
			return r, err
		}
	}
	r.Stage = initialActive
	return r, s.saveInitial(r)
}

func (s *Service) moveExecution(ctx context.Context, id, operationID, destination string) (Status, error) {
	unlock, err := s.lockSession(id)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	if !validID(operationID) || s.placements[destination] == nil {
		return Status{}, errors.New("home: canonical operation UUID and configured destination required")
	}
	r, err := s.readInitial(id)
	if err != nil {
		return Status{}, err
	}
	ctx, finish, err := s.graceful.Begin(ctx, id)
	if err != nil {
		return Status{}, err
	}
	defer finish()
	r, err = s.prepareInitialMove(ctx, r, operationID, destination)
	if err != nil {
		return Status{}, err
	}
	if err = s.checkFirstMove(r, operationID); err != nil {
		return Status{}, err
	}
	if err = ctx.Err(); err != nil {
		return Status{}, err
	}
	record, err := s.coordinator.Move(ctx, id, operationID, destination)
	out := Status{ID: id, InitialStage: r.Stage, ThreadID: record.ThreadID, Deadline: r.Launch.Deadline, Session: &record}
	return out, err
}

func (s *Service) prepareInitialMove(ctx context.Context, r initial, operationID, destination string) (initial, error) {
	var err error
	if r.Stage == initialStopped {
		r, err = s.importInitial(ctx, r)
		if err != nil {
			return r, err
		}
		r.OperationID = operationID
		r.Destination = destination
		if err = s.saveInitial(r); err != nil {
			return r, err
		}
	}
	if r.Stage == initialPreparing {
		r, err = s.start(ctx, r)
		if err != nil {
			return r, err
		}
	}
	if r.Stage == initialActive {
		r.Stage = initialImporting
		r.OperationID = operationID
		r.Destination = destination
		if err = s.saveInitial(r); err != nil {
			return r, err
		}
	}
	if r.Stage == initialImporting {
		if r.OperationID != operationID || r.Destination != destination {
			return r, errors.New("home: first move requires retained operation recovery")
		}
		r, err = s.importInitial(ctx, r)
		if err != nil {
			return r, err
		}
	}
	if r.Stage != initialImported {
		return r, errors.New("home: cancelled initial session cannot move")
	}
	return r, nil
}

func (s *Service) importInitial(ctx context.Context, r initial) (initial, error) {
	if r.Incarnation == nil {
		return r, errors.New("home: initial incarnation unavailable")
	}
	stopped, err := s.local.Stop(ctx, r.Launch, *r.Incarnation)
	if err != nil {
		return r, err
	}
	if !stopped.ActualStopped || stopped.Receipt == nil || !validID(stopped.Launch.ThreadID) {
		return r, errors.New("home: initial stop or usage remains unknown")
	}
	if _, err = s.coordinator.Create(ctx, session.Start{Source: stopped.Launch, Incarnation: *r.Incarnation, Stopped: stopped}); err != nil {
		return r, err
	}
	r.Launch = stopped.Launch
	r.Stage = initialImported
	return r, s.saveInitial(r)
}

func (s *Service) recoverExecution(ctx context.Context, id string) (Status, error) {
	unlock, err := s.lockSession(id)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	r, err := s.readInitial(id)
	if err != nil {
		return Status{}, err
	}
	ctx, finish, err := s.recoveryContext(ctx, r)
	if err != nil {
		return Status{}, err
	}
	defer finish()
	switch r.Stage {
	case initialPreparing:
		r, err = s.start(ctx, r)
	case initialImporting:
		if r.Destination == "" {
			_, err = s.stopInitial(ctx, r, r.OperationID)
			if err == nil {
				r, err = s.readInitial(id)
			}
		} else {
			r, err = s.importInitial(ctx, r)
			if err == nil {
				_, err = s.coordinator.Move(ctx, id, r.OperationID, r.Destination)
			}
		}
	case initialCancelling:
		r, err = s.cancelInitial(ctx, r)
	case initialImported:
		var record session.Record
		record, err = s.coordinator.Inspect(id)
		if err == nil && r.Destination != "" && record.CancelOperation == "" && !hasOperation(record, r.OperationID) {
			_, err = s.coordinator.Move(ctx, id, r.OperationID, r.Destination)
		} else if err == nil {
			_, err = s.coordinator.Recover(ctx, id)
		}
	}
	if err != nil {
		return Status{}, err
	}
	return s.status(r)
}

// Recovering persisted cancellation uses its own context. Only graceful work is
// registered for interruption, including the pre-import initial visit.
func (s *Service) recoveryContext(ctx context.Context, r initial) (context.Context, func(), error) {
	if r.Stage == initialCancelling || r.Stage == initialCancelled {
		return ctx, func() {}, nil
	}
	if r.Stage == initialImported {
		record, err := s.coordinator.Inspect(r.Launch.SessionID)
		if err != nil {
			return nil, nil, err
		}
		if record.CancelOperation != "" {
			return ctx, func() {}, nil
		}
	}
	return s.graceful.Begin(ctx, r.Launch.SessionID)
}

func hasOperation(r session.Record, id string) bool {
	for _, op := range r.Operations {
		if op.ID == id {
			return true
		}
	}
	return false
}
func (s *Service) checkFirstMove(r initial, id string) error {
	record, err := s.coordinator.Inspect(r.Launch.SessionID)
	if err != nil {
		return err
	}
	if r.Destination != "" && !hasOperation(record, r.OperationID) && id != r.OperationID {
		return errors.New("home: first imported move requires retained operation recovery")
	}
	return nil
}

func (s *Service) cancelExecution(ctx context.Context, id, operationID string) (Status, error) {
	if !validID(id) || !validID(operationID) {
		return Status{}, errors.New("home: canonical session and cancellation UUID required")
	}
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	finish, err := s.graceful.RequestCancel(id, operationID)
	if err != nil {
		return Status{}, err
	}
	defer finish()
	unlock, err := s.lockSession(id)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	r, err := s.readInitial(id)
	if err != nil {
		return Status{}, err
	}
	if r.Stage == initialImported {
		record, cancelErr := s.coordinator.Cancel(ctx, id, operationID)
		out := Status{ID: id, InitialStage: r.Stage, ThreadID: r.Launch.ThreadID, Deadline: r.Launch.Deadline, Session: &record}
		return out, cancelErr
	}
	if r.Stage == initialCancelling || r.Stage == initialCancelled {
		if r.OperationID != operationID {
			return Status{}, errors.New("home: cancellation intent conflicts")
		}
	} else {
		r.Stage = initialCancelling
		r.OperationID = operationID
		r.Destination = ""
		if err = s.saveInitial(r); err != nil {
			return Status{}, err
		}
	}
	if r.Stage != initialCancelled {
		r, err = s.cancelInitial(ctx, r)
		if err != nil {
			return Status{}, err
		}
	}
	return s.status(r)
}
func (s *Service) cancelInitial(ctx context.Context, r initial) (initial, error) {
	var instance session.Incarnation
	if r.Incarnation != nil {
		// The exact incarnation was retained before activation. Inspect can replay
		// an interrupted activation; cancellation must bypass that recovery path.
		instance = *r.Incarnation
	} else {
		if r.Input == checkpointInput {
			observation, err := s.local.Inspect(ctx, r.Launch)
			if err != nil {
				return r, err
			}
			if observation.State != session.PlacementAbsent {
				if observation.Incarnation.ResourceUID == "" {
					return r, errors.New("home: resumed preparation stop remains unknown")
				}
				instance = observation.Incarnation
			} else {
				r.Stage = initialCancelled
				return r, s.saveInitial(r)
			}
		} else {
			if err := s.local.CancelInitial(ctx, r.Launch); err != nil {
				return r, err
			}
			r.Stage = initialCancelled
			return r, s.saveInitial(r)
		}
	}
	if instance.ResourceUID == "" {
		return r, errors.New("home: cancellation stop remains unknown")
	}
	observation, err := s.local.Abort(ctx, r.Launch, instance)
	if err != nil {
		return r, err
	}
	stopped := observation.Stopped
	if observation.State != session.PlacementStopped || observation.Incarnation != instance ||
		stopped == nil || !stopped.ActualStopped || stopped.Incarnation != instance {
		return r, errors.New("home: cancellation stop remains unknown")
	}
	r.Stage = initialCancelled
	return r, s.saveInitial(r)
}

func (s *Service) attachExecution(ctx context.Context, id string, input io.Reader, output io.Writer) error {
	unlock, err := s.lockSession(id)
	if err != nil {
		return err
	}
	r, err := s.readInitial(id)
	var l session.Launch
	var i session.Incarnation
	var terminal session.TerminalPlacement
	if err == nil {
		if r.Stage == initialActive && r.Incarnation != nil {
			l = r.Launch
			i = *r.Incarnation
			terminal = s.local
		} else if r.Stage == initialImported {
			var record session.Record
			record, err = s.coordinator.Inspect(id)
			if err == nil && record.Active != nil && record.Phase == session.PhaseActive {
				l = record.Active.Launch
				i = record.Active.Incarnation
				terminal, _ = s.placements[l.Placement].(session.TerminalPlacement)
			}
		}
	}
	unlock()
	if err != nil {
		return err
	}
	if terminal == nil {
		return errors.New("home: native attachment is unavailable")
	}
	return terminal.Attach(ctx, l, i, input, output)
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
