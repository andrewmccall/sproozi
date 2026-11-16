package home

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

// Reconciling on read: a command that holds a session's lock first brings
// the session's catalogue entry up to what the execution journal records.
// retireSettled retires an idle current execution, a stop whose catalogue
// write failed after the journal recorded it, and releaseOrphans releases the
// executions of the session that its entry never recorded. Reads derive the
// same state without writing (derive), so reconciling changes what a session
// keeps, never what it reads as. Neither starts work, refunds usage or
// cancels an execution the session recorded, so neither ends nor resurrects
// a session, and repeating either changes nothing more.

// releaseOrphans cancels and removes every execution that belongs to session
// r but that r's catalogue entry never recorded. Resume retains a new
// execution, then records it in the catalogue before preparing it; a resume
// interrupted between the two leaves an execution nothing can reach: commands
// on its ID are refused because the session does not name it, and no listing
// shows it, yet it holds one of the coordinator's retention slots.
//
// Such an execution never ran: it has no visit and no writer. Its input is
// the checkpoint the session kept, which the catalogue retains, so cancelling
// and removing it loses nothing; making it the session's current execution
// could replace a later stop with an older checkpoint. An execution of r that
// did run is left alone and reported. The caller holds r's lock, which every
// command that creates an execution for r also holds.
func (s *Service) releaseOrphans(ctx context.Context, r localSession) error {
	ids, err := s.coordinator.List()
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if id == r.ID || slices.Contains(r.Executions, id) {
			continue
		}
		execution, err := s.coordinator.Inspect(id)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if execution.Session != r.ID {
			continue
		}
		if execution.Active != nil || len(execution.Visits) != 0 {
			errs = append(errs, fmt.Errorf("home: execution %s of session %s ran, but the session never recorded "+
				"it; it is kept", id, r.ID))
			continue
		}
		if !session.Removable(execution) {
			// A cancellation an earlier attempt recorded continues with its
			// own operation.
			operation := cmp.Or(execution.CancelOperation, uuid.NewString())
			if execution, err = s.coordinator.Cancel(ctx, id, operation, nil); err != nil {
				errs = append(errs, fmt.Errorf("home: cancelling execution %s that session %s never recorded: %w",
					id, r.ID, err))
				continue
			}
		}
		if !session.Removable(execution) {
			continue
		}
		if err = s.coordinator.Remove([]string{id}, s.catalogTexts); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// logf writes an operator message to the service log, if it has one.
func (s *Service) logf(format string, args ...any) {
	if s.log != nil {
		_, _ = fmt.Fprintf(s.log, format, args...)
	}
}
