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
		if _, _, err = s.resolveSession(id, ActionStop); err != nil {
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
	if err = s.refuseIn(r, ActionStop); err != nil {
		return Status{}, err
	}
	// A stop the journal recorded but this entry did not is the stop asked
	// for: retire it, and the request replays as for any stopped session.
	if err = s.retireSettled(&r); err != nil {
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
	execution, err := s.coordinator.Finalize(ctx, r.Current, operation)
	if err != nil {
		return Status{}, err
	}
	if err = s.retire(&r, execution); err != nil {
		return Status{}, err
	}
	return s.catalogStatus(r)
}

// retire commits what a session keeps from its current execution once that
// execution is idle, and saves the session as stopped. The caller holds the
// session's lock.
func (s *Service) retire(r *localSession, execution session.Record) error {
	stopped, err := s.settledStop(*r, execution)
	if err != nil {
		return err
	}
	// A stop the session already kept has its artifact retained.
	if stopped.CheckpointDigest != "" && stopped.CheckpointDigest != r.Committed.CheckpointDigest {
		if err = s.retainCatalogArtifact(stopped.Checkpoint, stopped.CheckpointDigest); err != nil {
			return err
		}
	}
	r.Committed = stopped
	r.Current = ""
	return s.saveCatalog(*r)
}

// retireSettled retires the session's current execution if it is idle and
// not cancelled: a recovery finished its stop, or its deadline ended its start
// or move (ADR 0060). Anything else stays current. The caller holds the
// session's lock.
func (s *Service) retireSettled(r *localSession) error {
	st, current, err := s.sessionState(*r)
	if err != nil || current == nil || !st.stopped() {
		return err
	}
	return s.retire(r, *current)
}

// retireSettledSession is retireSettled for a session the caller has locked
// by ID; an execution without a session has nothing to retire.
func (s *Service) retireSettledSession(id string) error {
	if id == "" {
		return nil
	}
	r, err := s.readCatalog(id)
	if err != nil {
		return err
	}
	return s.retireSettled(&r)
}

// endLate ends the session's current start or move once its deadline has
// passed and retires the idle execution, so resume can continue the session
// from what it kept (ADR 0060). A destination that did become active stays
// current. The caller holds the session's lock.
func (s *Service) endLate(ctx context.Context, r *localSession) error {
	st, execution, err := s.sessionState(*r)
	if err != nil {
		return err
	}
	if st.overdue {
		if _, err = s.coordinator.Recover(ctx, execution.ID); err != nil && !errors.Is(err, session.ErrDeadlinePassed) {
			return err
		}
	}
	return s.retireSettled(r)
}

// settledStop is what a session keeps from an idle execution: its last
// stopped visit and checkpoint. An execution whose deadline ended its first
// visit before any visit ran leaves the session's earlier stop, or, for a
// session whose first execution never ran, the workspace it was started
// from, which resume restores.
func (s *Service) settledStop(r localSession, execution session.Record) (session.Stopped, error) {
	switch {
	case len(execution.Visits) != 0:
		return s.lastStop(execution)
	case r.Committed.Launch.ExecutionID != "":
		return r.Committed, nil
	}
	return session.Stopped{Launch: *execution.Operations[0].Launch}, nil
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
