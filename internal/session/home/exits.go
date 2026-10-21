package home

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

// exitObserver is implemented by placements that can tell when an active
// visit's harness exited on its own.
type exitObserver interface {
	Exited(context.Context, session.Launch, session.Incarnation) (bool, error)
}

// exitWatchInterval is how often the service looks for harnesses that exited
// on their own.
var exitWatchInterval = 5 * time.Second

// watchExits stops, through the ordinary stop path, every active session whose
// harness exited on its own, so its status shows the exit instead of staying
// active and its usage is finalized.
func (s *Service) watchExits(ctx context.Context) {
	ticker := time.NewTicker(exitWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		dir, err := s.catalogDir()
		if err != nil {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if id, ok := strings.CutSuffix(entry.Name(), ".json"); ok {
				_, _ = s.settleExit(ctx, id)
			}
		}
	}
}

// settleExit stops a catalogued session whose current active visit's harness
// exited on its own. It reports whether it did.
func (s *Service) settleExit(ctx context.Context, id string) (bool, error) {
	r, err := s.readCatalog(id)
	if err != nil || r.Current == "" {
		return false, err
	}
	execution, err := s.coordinator.Inspect(r.Current)
	if err != nil || execution.Phase != session.PhaseActive || execution.Active == nil {
		return false, err
	}
	observer, ok := s.placement(execution.Active.Launch.Placement).(exitObserver)
	if !ok {
		return false, nil
	}
	exited, err := observer.Exited(ctx, execution.Active.Launch, execution.Active.Incarnation)
	if err != nil || !exited {
		return false, err
	}
	_, err = s.Stop(ctx, id, uuid.NewString())
	return err == nil, err
}

// stageFailed is a stopped session whose harness exited with a failure.
const stageFailed = "failed"

// committedStage names a stopped session's phase: failed when its last visit's
// harness exited on its own with a nonzero code, otherwise retained.
func committedStage(r localSession) string {
	if exit := r.Committed.Exit; exit != nil && exit.Code != 0 {
		return stageFailed
	}
	return stageRetained
}

// stoppedProblem explains a command that needs a running session, naming the
// next command for a stopped one.
func stoppedProblem(r localSession) error {
	if exit := r.Committed.Exit; exit != nil && exit.Code != 0 {
		return rejectedError(fmt.Sprintf("home: session %s stopped because its agent exited with code %d; "+
			"see \"sproozi status %s\", then \"sproozi resume %s\" to continue it or \"sproozi rm %s\" to remove it",
			r.ID, exit.Code, r.ID, r.ID, r.ID))
	}
	return rejectedError(fmt.Sprintf("home: session %s is stopped; \"sproozi resume %s\" continues it", r.ID, r.ID))
}

// notStoppedProblem explains why a session whose current execution has not
// stopped cannot resume, naming the next command for its phase.
func (s *Service) notStoppedProblem(id, current string) error {
	r, err := s.coordinator.Inspect(current)
	if err != nil {
		return err
	}
	switch {
	case r.Phase == session.PhaseCancelled:
		return cancelledProblem(id)
	case r.CancelOperation != "":
		return rejectedError(fmt.Sprintf("home: session %s is being cancelled; run \"sproozi recover %s\" to finish "+
			"the cancellation, then \"sproozi rm %s\" to remove it", id, id, id))
	case r.Phase == session.PhaseActive:
		return rejectedError(fmt.Sprintf("home: session %s is running; attach to it with \"sproozi attach %s\", or "+
			"stop it with \"sproozi stop %s\" before resuming", id, id, id))
	case r.Phase == session.PhaseStopping || r.Phase == session.PhaseUnknownStop || r.Phase == session.PhaseUnknownUsage:
		return rejectedError(fmt.Sprintf("home: session %s is still stopping; run \"sproozi stop %s\" to finish, "+
			"or \"sproozi cancel %s\" to force-stop it", id, id, id))
	}
	return rejectedError(fmt.Sprintf("home: session %s is still starting or moving; run \"sproozi recover %s\" "+
		"to finish that first", id, id))
}

// attachProblem refuses an attachment, before the terminal opens, to a
// session whose agent is not running, and says what to do instead.
func (s *Service) attachProblem(ctx context.Context, id string) error {
	if r, err := s.readCatalog(id); err == nil {
		if r.Current == "" {
			return stoppedProblem(r)
		}
		if settled, _ := s.settleExit(ctx, id); settled {
			if r, err = s.readCatalog(id); err == nil {
				return stoppedProblem(r)
			}
		}
		execution, err := s.coordinator.Inspect(r.Current)
		if err != nil {
			return err
		}
		if execution.Phase != session.PhaseActive {
			return s.notStoppedProblem(id, r.Current)
		}
	}
	return nil
}
