package home

import (
	"context"
	"errors"
	"os"

	"github.com/andrewmccall/sproozi/internal/session"
)

// Stop commits selected native/workspace continuity without preparing another
// writer. Durable sessions retain it independently of the finished execution.
func (s *Service) Stop(ctx context.Context, id, operation string) (Status, error) {
	if !validID(operation) {
		return Status{}, errors.New("home: stop operation UUID required")
	}
	if execution, err := s.coordinator.Inspect(id); err == nil && execution.Session != "" {
		if _, _, err = s.resolveSession(id); err != nil {
			return Status{}, err
		}
		id = execution.Session
	}
	r, err := s.readCatalog(id)
	if os.IsNotExist(err) {
		execution, finalizeErr := s.coordinator.Finalize(ctx, id, operation)
		if finalizeErr != nil {
			return Status{}, finalizeErr
		}
		return executionStatus(execution), nil
	}
	if err != nil {
		return Status{}, err
	}
	unlock, err := s.lockSession(id)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	r, err = s.readCatalog(id)
	if err != nil {
		return Status{}, err
	}
	if r.Current == "" {
		if previous, exists := r.Operations[operation]; exists {
			if previous.Action != ActionStop {
				return Status{}, errors.New("home: stop operation intent conflicts")
			}
		} else if err = s.bindCatalogOperation(&r, operation, catalogOperation{Action: ActionStop}); err != nil {
			return Status{}, err
		}
		return s.catalogStatus(r)
	}
	if err = s.bindCatalogOperation(&r, operation, catalogOperation{Action: ActionStop, Execution: r.Current}); err != nil {
		return Status{}, err
	}
	stopped, err := s.stopExecution(ctx, r.Current, operation)
	if err != nil {
		return Status{}, err
	}
	if err = s.retainCatalogArtifact(stopped.Checkpoint, stopped.CheckpointDigest); err != nil {
		return Status{}, err
	}
	r.Committed = stopped
	r.Current = ""
	if err = s.saveCatalog(r); err != nil {
		return Status{}, err
	}
	return s.catalogStatus(r)
}

// stopExecution finalizes an execution and returns its last stopped visit with
// the execution's committed checkpoint.
func (s *Service) stopExecution(ctx context.Context, id, operation string) (session.Stopped, error) {
	r, err := s.coordinator.Finalize(ctx, id, operation)
	if err != nil {
		return session.Stopped{}, err
	}
	if len(r.Visits) == 0 {
		return session.Stopped{}, errors.New("home: execution has no stopped visit")
	}
	data, err := s.coordinator.Checkpoint(id)
	if err != nil {
		return session.Stopped{}, err
	}
	visit := r.Visits[len(r.Visits)-1]
	return session.Stopped{Launch: visit.Launch, Incarnation: visit.Incarnation, ActualStopped: visit.ActualStopped,
		ThreadID: r.ThreadID, Receipt: visit.Receipt, Checkpoint: data, CheckpointDigest: r.Checkpoint.Metadata.SHA256}, nil
}
