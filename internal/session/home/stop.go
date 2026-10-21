package home

import (
	"context"
	"errors"
	"os"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/work"
)

// Stop commits selected native/workspace continuity without preparing another
// writer. Durable sessions retain it independently of the finished execution.
func (s *Service) Stop(ctx context.Context, id, operation string) (Status, error) {
	if !work.CanonicalID(operation) {
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
	if stopped.CheckpointDigest != "" {
		if err = s.retainCatalogArtifact(stopped.Checkpoint, stopped.CheckpointDigest); err != nil {
			return Status{}, err
		}
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
	return s.lastStop(r)
}

// lastStop is an idle execution's last stopped visit with the execution's
// committed checkpoint. A session stopped with no conversation has none: it
// resumes from the workspace its execution started from.
func (s *Service) lastStop(r session.Record) (session.Stopped, error) {
	if len(r.Visits) == 0 {
		return session.Stopped{}, errors.New("home: execution has no stopped visit")
	}
	var data []byte
	if r.Checkpoint.Path != "" {
		var err error
		if data, err = s.coordinator.Checkpoint(r.ID); err != nil {
			return session.Stopped{}, err
		}
	}
	visit := r.Visits[len(r.Visits)-1]
	return session.Stopped{Exit: visit.Exit, Launch: visit.Launch, Incarnation: visit.Incarnation, ActualStopped: visit.ActualStopped,
		ThreadID: r.ThreadID, Receipt: visit.Receipt, Checkpoint: data, CheckpointDigest: r.Checkpoint.Metadata.SHA256}, nil
}
