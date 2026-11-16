package home

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

// exitWatchInterval is how often the service looks for harnesses that exited
// on their own.
var exitWatchInterval = 5 * time.Second

// exitCheckTimeout bounds one session's question to its placement, so an
// executor that cannot be reached answers within it, and holds the remote
// client's connection lock no longer, while the stop of an exited visit takes
// as long as a stop takes.
const exitCheckTimeout = 10 * time.Second

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
		s.sweepExits(ctx)
	}
}

// sweepExits checks every catalogued session for an exited agent, each in
// its own goroutine, so one placement that is slow or unreachable delays no
// other session's check. A session whose previous check still runs is
// skipped. The returned channel closes once the checks this sweep started
// have finished.
func (s *Service) sweepExits(ctx context.Context) <-chan struct{} {
	var checks sync.WaitGroup
	done := make(chan struct{})
	defer func() {
		go func() {
			checks.Wait()
			close(done)
		}()
	}()
	dir, err := s.catalogDir()
	if err != nil {
		return done
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return done
	}
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok {
			continue
		}
		if _, running := s.exitChecks.LoadOrStore(id, struct{}{}); running {
			continue
		}
		checks.Go(func() {
			defer s.exitChecks.Delete(id)
			_, err := s.settleExit(ctx, id)
			if ctx.Err() == nil {
				s.noteExitCheck(id, err)
			}
		})
	}
	return done
}

// noteExitCheck logs a session's failed exit check once each time its reason
// changes, and once when checks succeed again, rather than on every sweep.
func (s *Service) noteExitCheck(id string, err error) {
	if err == nil {
		if _, failed := s.exitProblems.LoadAndDelete(id); failed && s.log != nil {
			_, _ = fmt.Fprintf(s.log, "Checked session %s for an exited agent again\n", id)
		}
		return
	}
	reason := err.Error()
	if previous, failed := s.exitProblems.Swap(id, reason); failed && previous == reason {
		return
	}
	if s.log != nil {
		_, _ = fmt.Fprintf(s.log, "Failed to check session %s for an exited agent: %v\n", id, err)
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
	// The Docker placement and a remote executor's client observe exits; a
	// remote executor older than the exited action reports none.
	observer, ok := s.placement(execution.Active.Launch.Placement).(session.ExitObserver)
	if !ok {
		return false, nil
	}
	check, cancel := context.WithTimeout(ctx, exitCheckTimeout)
	exited, err := observer.Exited(check, execution.Active.Launch, execution.Active.Incarnation)
	cancel()
	if err != nil || !exited {
		return false, err
	}
	_, err = s.Stop(ctx, id, uuid.NewString())
	return err == nil, err
}

// stageFailed is a stopped session whose harness exited with a failure.
const stageFailed = "failed"

// committedStage names a stopped session's phase: failed when its last visit's
// harness exited on its own with a nonzero code before its deadline,
// otherwise retained. A harness its supervisor stopped is retained.
func committedStage(r localSession) string {
	if r.Committed.Exit.Failed() {
		return stageFailed
	}
	return stageRetained
}

// stoppedProblem explains a command that needs a running session, naming the
// next command for a stopped one.
func stoppedProblem(r localSession) error {
	if exit := r.Committed.Exit; exit.Failed() {
		return rejectedError(fmt.Sprintf("home: session %s stopped because its agent exited with code %d; "+
			"see \"sproozi status %s\", then \"sproozi resume %s\" to continue it or \"sproozi rm %s\" to remove it",
			r.ID, exit.Code, r.ID, r.ID, r.ID))
	}
	return rejectedError(fmt.Sprintf("home: session %s is retained (stopped, with its work kept); \"sproozi resume %s\" continues it", r.ID, r.ID))
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
	case r.Phase.Stopping():
		return rejectedError(fmt.Sprintf("home: session %s is still stopping; run \"sproozi stop %s\" to finish, "+
			"or \"sproozi cancel %s\" to force-stop it", id, id, id))
	case r.Phase.Starting() && s.coordinator.PastDeadline(r):
		// resume ends a start or move its deadline overtook, and continues.
		return rejectedError(fmt.Sprintf("home: session %s reached its deadline before it started; "+
			"\"sproozi resume %s\" ends that start and continues the session with a new deadline, or, if the "+
			"placement cannot show that nothing runs there, \"sproozi cancel %s\" records the cancellation and "+
			"finishes it once the placement can show that", id, id, id))
	case r.Phase == session.PhaseIdle:
		return rejectedError(fmt.Sprintf("home: session %s is stopped, with its work kept; \"sproozi resume %s\" "+
			"continues it", id, id))
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
