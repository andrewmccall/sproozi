package home

import (
	"context"
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
	var entries []os.DirEntry
	if err == nil {
		entries, err = os.ReadDir(dir)
	}
	if ctx.Err() == nil {
		s.noteProblem(sweepProblem, err, "Failed to list sessions to check for exited agents: %v\n",
			"Listed sessions to check for exited agents again\n")
	}
	if err != nil {
		return done
	}
	catalogued := make(map[string]bool, len(entries))
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok {
			continue
		}
		catalogued[id] = true
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
	// A session removed while its check was failing logs nothing more.
	s.exitProblems.Range(func(id, _ any) bool {
		if key := id.(string); key != sweepProblem && !strings.HasPrefix(key, journalProblem) && !catalogued[key] {
			s.exitProblems.Delete(id)
		}
		return true
	})
	return done
}

// sweepProblem keys exitProblems' entry for a sweep that could not list the
// catalogue; session UUIDs key the rest.
const sweepProblem = ""

// noteExitCheck logs a session's failed exit check once each time its reason
// changes, and once when checks succeed again, rather than on every sweep.
// The check runs in the background, so the service log is where its errors
// go; a stop it starts and cannot finish leaves the session in the phase its
// journal records, which status reports.
func (s *Service) noteExitCheck(id string, err error) {
	s.noteProblem(id, err, "Failed to check session "+id+" for an exited agent: %v\n",
		"Checked session "+id+" for an exited agent again\n")
}

// noteProblem logs failure, with err, once each time key's error changes,
// and recovery once when key next succeeds.
func (s *Service) noteProblem(key string, err error, failure, recovery string) {
	if err == nil {
		if _, failed := s.exitProblems.LoadAndDelete(key); failed {
			s.logf("%s", recovery)
		}
		return
	}
	reason := err.Error()
	if previous, failed := s.exitProblems.Swap(key, reason); failed && previous == reason {
		return
	}
	s.logf(failure, err)
}

// settleExit stops a catalogued session whose current active visit's harness
// exited on its own. It reports whether it did.
func (s *Service) settleExit(ctx context.Context, id string) (bool, error) {
	r, err := s.readCatalog(id)
	if err != nil {
		return false, err
	}
	st, execution, err := s.sessionState(r)
	if err != nil || st.phase != Phase(session.PhaseActive) {
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

// attachProblem refuses an attachment, before the terminal opens, to a
// session whose agent is not running, and says what to do instead. An agent
// that exited by itself is stopped first, so the refusal names the exit.
func (s *Service) attachProblem(ctx context.Context, id string) error {
	r, err := s.readCatalog(id)
	if os.IsNotExist(err) {
		// An execution's ID: its session's state decides, as for the
		// session's own ID, and one the session no longer runs is refused
		// before the terminal opens.
		_, _, err = s.resolveSession(id, ActionAttach)
		return err
	}
	if err != nil {
		// Attach reports an unreadable session itself.
		return nil
	}
	st, _, err := s.sessionState(r)
	if err != nil {
		return err
	}
	if st.phase == Phase(session.PhaseActive) {
		settled, err := s.settleExit(ctx, id)
		if ctx.Err() == nil {
			s.noteExitCheck(id, err)
		}
		if settled {
			if r, err = s.readCatalog(id); err != nil {
				return err
			}
			if st, _, err = s.sessionState(r); err != nil {
				return err
			}
		}
	}
	return refusal(id, st, ActionAttach)
}
