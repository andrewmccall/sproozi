package home

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/andrewmccall/sproozi/internal/session"
)

// Stop commits selected native/workspace continuity without preparing another
// writer. Product sessions retain it independently of the finished envelope.
func (s *Service) Stop(ctx context.Context, id, operation string) (Status, error) {
	if !validID(operation) {
		return Status{}, errors.New("home: stop operation UUID required")
	}
	if initial, err := s.readInitial(id); err == nil && initial.ProductID != "" {
		if _, _, err = s.resolveSession(id); err != nil {
			return Status{}, err
		}
		id = initial.ProductID
	}
	r, err := s.readCatalog(id)
	if os.IsNotExist(err) {
		_, err = s.stopExecution(ctx, id, operation)
		if err != nil {
			return Status{}, err
		}
		return s.executionStatus(id)
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

func (s *Service) stopExecution(ctx context.Context, id, operation string) (session.Stopped, error) {
	unlock, err := s.lockSession(id)
	if err != nil {
		return session.Stopped{}, err
	}
	defer unlock()
	r, err := s.readInitial(id)
	if err != nil {
		return session.Stopped{}, err
	}
	ctx, finish, err := s.graceful.Begin(ctx, id)
	if err != nil {
		return session.Stopped{}, err
	}
	defer finish()
	if r.Stage == initialStopped {
		if r.OperationID != operation || r.Stopped == nil {
			return session.Stopped{}, errors.New("home: retained stop operation conflicts")
		}
		stopped := *r.Stopped
		stopped.Checkpoint, err = readPrivate(filepath.Join(s.initialDir, id+".stopped"), s.config.Local.MaxCheckpointBytes)
		return stopped, err
	}
	if r.Stage == initialImported {
		record, finalizeErr := s.coordinator.Finalize(ctx, id, operation)
		if finalizeErr != nil {
			return session.Stopped{}, finalizeErr
		}
		visit := record.Visits[len(record.Visits)-1]
		data, readErr := readPrivate(filepath.Join(s.config.Directory, "journal", record.Checkpoint.Path), s.config.Local.MaxCheckpointBytes)
		if readErr != nil {
			return session.Stopped{}, readErr
		}
		return session.Stopped{Launch: visit.Launch, Incarnation: visit.Incarnation, ActualStopped: visit.ActualStopped,
			Receipt: visit.Receipt, Checkpoint: data, CheckpointDigest: record.Checkpoint.Metadata.SHA256}, nil
	}
	return s.stopInitial(ctx, r, operation)
}

func (s *Service) stopInitial(ctx context.Context, r initial, operation string) (session.Stopped, error) {
	if r.Stage != initialActive && r.Stage != initialImporting {
		return session.Stopped{}, errors.New("home: execution is not active or finalizing")
	}
	if r.Stage == initialImporting && (r.OperationID != operation || r.Destination != "") {
		return session.Stopped{}, errors.New("home: previous move requires recovery")
	}
	if r.Incarnation == nil {
		return session.Stopped{}, errors.New("home: stopped incarnation unavailable")
	}
	r.Stage = initialImporting
	r.OperationID = operation
	r.Destination = ""
	if err := s.saveInitial(r); err != nil {
		return session.Stopped{}, err
	}
	stopped, err := s.local.Stop(ctx, r.Launch, *r.Incarnation)
	if err != nil {
		return session.Stopped{}, err
	}
	id := r.Launch.SessionID
	e := exportFromStopped(stopped, id, operation)
	if err = s.validateExport(e, id, operation); err != nil {
		return session.Stopped{}, err
	}
	if stopped.Incarnation != *r.Incarnation || stopped.Launch.SessionID != id ||
		stopped.Launch.AllocationID != r.Launch.AllocationID {
		return session.Stopped{}, errors.New("home: stopped allocation conflicts")
	}
	if err = s.writeInitial(id+".stopped", stopped.Checkpoint); err != nil {
		return session.Stopped{}, err
	}
	r.Stopped = &stopped
	r.Launch = stopped.Launch
	r.Stage = initialStopped
	if err = s.saveInitial(r); err != nil {
		return session.Stopped{}, err
	}
	return stopped, nil
}
