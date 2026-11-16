package home

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

// orphanOf returns the one execution of id that its catalogue entry never
// recorded.
func orphanOf(t *testing.T, s *Service, id string) string {
	t.Helper()
	c, err := s.readCatalog(id)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := s.coordinator.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ids {
		if e != id && !slices.Contains(c.Executions, e) {
			return e
		}
	}
	t.Fatal("no unrecorded execution")
	return ""
}

// A journal that cannot be read, such as one a newer release wrote with a
// field this one does not know, and that cannot be shown to belong to the
// session, does not stop rm of an unrelated session. It is logged.
func TestRemoveSkipsAnUnreadableJournalOfAnotherSession(t *testing.T) {
	t.Parallel()
	s, _, _, repo := testService(t)
	var log strings.Builder
	s.log = &log
	id := startedOn(t, s, repo, testLocal)
	if _, err := s.Stop(t.Context(), id, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	unreadable := uuid.NewString()
	bad := filepath.Join(s.config.Directory, "journal", unreadable+".json")
	if err := os.WriteFile(bad, []byte(`{"version":99,"futureField":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if r := exchange(t, s, Request{Action: ActionRemove, ID: id}); r.Error != "" {
		t.Fatalf("rm of an unrelated, stopped session replied %+v", r)
	}
	if _, err := os.Stat(bad); err != nil {
		t.Fatalf("the unreadable journal was not kept: %v", err)
	}
	if !strings.Contains(log.String(), "Failed to read execution "+unreadable) {
		t.Fatalf("log: %q", log.String())
	}
}

// interruptedStopOfAResume stops a started session, resumes it, runs then
// when non-nil, and stops it while the catalogue fills, so the journal
// records the stop but the catalogue still names the resumed execution as
// current. It returns that execution, which differs from the session.
func interruptedStopOfAResume(t *testing.T, s *Service, local *placementFixture, id string, then func()) string {
	t.Helper()
	if _, err := s.Stop(t.Context(), id, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	resumed, err := s.Resume(t.Context(), id, uuid.NewString(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if then != nil {
		then()
	}
	release := func() {}
	local.onStop = func() { release = fillCatalogue(t, s) }
	if _, err = s.Stop(t.Context(), id, uuid.NewString()); err == nil {
		t.Fatal("stop saved its catalogue entry past the catalogue's limit")
	}
	release()
	if c, err := s.readCatalog(id); err != nil || c.Current != resumed.ExecutionID || resumed.ExecutionID == id {
		t.Fatalf("the catalogue recorded the stop: %+v %v", c, err)
	}
	return resumed.ExecutionID
}

// A command that names the session's current execution rather than the
// session is refused in the same words as one that names the session.
func TestCommandsByExecutionIDFollowTheTable(t *testing.T) {
	t.Parallel()
	for _, action := range []string{ActionCancel, ActionMove, ActionRecover, ActionAttach} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			s, local, remote, repo := testService(t)
			id := startedOn(t, s, repo, testLocal)
			execution := interruptedStopOfAResume(t, s, local, id, nil)
			request := func(target string) Request {
				q := Request{Action: action, ID: target}
				switch action {
				case ActionMove:
					q.OperationID, q.Destination = uuid.NewString(), testRemote
				case ActionCancel:
					q.OperationID = uuid.NewString()
				}
				return q
			}
			bySession := exchange(t, s, request(id))
			byExecution := exchange(t, s, request(execution))
			if !bySession.Rejected || !strings.Contains(bySession.Error, "is retained") ||
				byExecution.Error != bySession.Error || !byExecution.Rejected {
				t.Fatalf("by session %+v, by execution %+v", bySession, byExecution)
			}
			if x, err := s.coordinator.Inspect(execution); err != nil || x.Phase != session.PhaseIdle ||
				remote.prepares != 0 {
				t.Fatalf("the refused command changed the execution: %s %v", x.Phase, err)
			}
		})
	}
}

// ranOrphan makes the execution an interrupted resume left run and, when
// stop is set, stop with a checkpoint: no command of the service does this,
// but an execution of the session that ran must not be lost or strand it.
func ranOrphan(t *testing.T, s *Service, id string, stop bool) string {
	t.Helper()
	interruptedResume(t, s, id)
	orphan := orphanOf(t, s, id)
	if _, err := s.coordinator.Recover(t.Context(), orphan); err != nil {
		t.Fatal(err)
	}
	if stop {
		if _, err := s.coordinator.Finalize(t.Context(), orphan, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	return orphan
}

// An unrecorded execution that ran keeps its work: the session's rm refuses
// and names it, status shows it, and rm of it, after cancel while it still
// runs, removes it; then the session's rm works.
func TestAnUnrecordedExecutionThatRanKeepsItsWorkUntilRemoved(t *testing.T) {
	t.Parallel()
	for _, running := range []bool{false, true} {
		name := "stopped"
		if running {
			name = "running"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, _, _, repo := testService(t)
			id := startedOn(t, s, repo, testLocal)
			orphan := ranOrphan(t, s, id, !running)
			want := unrecordedProblem(id, orphan).Error()
			for range 2 {
				// Repeating the session's rm changes nothing.
				if r := exchange(t, s, Request{Action: ActionRemove, ID: id}); !r.Rejected || r.Error != want {
					t.Fatalf("rm of the session replied %+v, want %q", r, want)
				}
			}
			x, err := s.coordinator.Inspect(orphan)
			if err != nil || len(x.Visits)+btoi(x.Active != nil) == 0 {
				t.Fatalf("the execution that ran was not kept: %+v %v", x.Phase, err)
			}
			if r := exchange(t, s, Request{Action: ActionStatus, ID: orphan}); r.Error != "" || r.Status == nil ||
				r.Status.ExecutionID != orphan {
				t.Fatalf("status of the execution replied %+v", r)
			}
			if running {
				if r := exchange(t, s, Request{Action: ActionRemove, ID: orphan}); !r.Rejected {
					t.Fatalf("rm of the running execution replied %+v", r)
				}
				r := exchange(t, s, Request{Action: ActionCancel, ID: orphan, OperationID: uuid.NewString()})
				if r.Error != "" {
					t.Fatalf("cancel of the execution replied %+v", r)
				}
			}
			if r := exchange(t, s, Request{Action: ActionRemove, ID: orphan}); r.Error != "" {
				t.Fatalf("rm of the execution replied %+v", r)
			}
			if r := exchange(t, s, Request{Action: ActionRemove, ID: id}); r.Error != "" {
				t.Fatalf("rm of the session replied %+v", r)
			}
			if ids, err := s.coordinator.List(); err != nil || len(ids) != 0 {
				t.Fatalf("executions left: %v %v", ids, err)
			}
		})
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// A clean-up that recorded the unrecorded execution's cancellation but could
// not finish it, because the placement could not abort it, finishes on the
// next rm with the cancellation it recorded.
func TestAnInterruptedOrphanCleanupFinishesWithItsOwnCancellation(t *testing.T) {
	t.Parallel()
	s, local, _, repo := testService(t)
	id := startedOn(t, s, repo, testLocal)
	interruptedResume(t, s, id)
	orphan := orphanOf(t, s, id)
	o, err := s.coordinator.Inspect(orphan)
	if err != nil {
		t.Fatal(err)
	}
	// A placement row makes AbortPreparation call Abort, which then fails.
	l := *o.Operations[0].Launch
	local.rows[l.AllocationID] = session.Observation{Launch: l, State: session.PlacementPreparing,
		Incarnation: session.Incarnation{ExecutionID: l.AllocationID, ResourceUID: uuid.NewString()}}
	local.unreachableAbort = true
	if r := exchange(t, s, Request{Action: ActionRemove, ID: id}); r.Error == "" {
		t.Fatal("rm succeeded although the execution's abort failed")
	}
	if o, err = s.coordinator.Inspect(orphan); err != nil || o.CancelOperation == "" || o.Phase.Settled() {
		t.Fatalf("after the failed clean-up: %s %q %v", o.Phase, o.CancelOperation, err)
	}
	local.unreachableAbort = false
	delete(local.rows, l.AllocationID)
	if r := exchange(t, s, Request{Action: ActionRemove, ID: id}); r.Error != "" {
		t.Fatalf("the second rm replied %+v", r)
	}
	if ids, err := s.coordinator.List(); err != nil || len(ids) != 0 {
		t.Fatalf("executions left: %v %v", ids, err)
	}
}

// An unrecorded execution whose cancellation finished but whose journal was
// not deleted is deleted by the session's next resume.
func TestACancelledOrphanIsDeletedByTheNextResume(t *testing.T) {
	t.Parallel()
	s, _, _, repo := testService(t)
	id := startedOn(t, s, repo, testLocal)
	interruptedResume(t, s, id)
	orphan := orphanOf(t, s, id)
	if o, err := s.coordinator.Cancel(t.Context(), orphan, uuid.NewString(), nil); err != nil ||
		o.Phase != session.PhaseCancelled {
		t.Fatalf("%v %v", o.Phase, err)
	}
	if _, err := s.Resume(t.Context(), id, uuid.NewString(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.coordinator.Inspect(orphan); !os.IsNotExist(err) {
		t.Fatalf("the cancelled execution was kept: %v", err)
	}
}

// Resumes, one repeating the interrupted operation, rm and exit sweeps run
// together on a session with an unrecorded execution. Afterwards none of the
// session's executions is unrecorded, none it records is cancelled, and only
// the current one may be unsettled.
func TestConcurrentResumeRemoveAndSweepLeaveNoOrphan(t *testing.T) {
	t.Parallel()
	for range 4 {
		s, _, _, repo := testService(t)
		id := startedOn(t, s, repo, testLocal)
		interrupted := interruptedResume(t, s, id)
		ctx, cancel := context.WithCancel(t.Context())
		var wg sync.WaitGroup
		wg.Go(func() { _, _ = s.Resume(ctx, id, interrupted, nil) })
		wg.Go(func() { _, _ = s.Resume(ctx, id, uuid.NewString(), nil) })
		wg.Go(func() { _, _ = s.Remove(ctx, id) })
		wg.Go(func() {
			for range 5 {
				<-s.sweepExits(ctx)
			}
		})
		wg.Wait()
		cancel()
		ids, err := s.coordinator.List()
		if err != nil {
			t.Fatal(err)
		}
		c, err := s.readCatalog(id)
		removed := os.IsNotExist(err)
		if err != nil && !removed {
			t.Fatal(err)
		}
		for _, e := range ids {
			x, err := s.coordinator.Inspect(e)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case removed && (x.Session == id || e == id):
				t.Errorf("the removed session left execution %s (%s)", e, x.Phase)
			case removed:
			case e != id && !slices.Contains(c.Executions, e):
				t.Errorf("unrecorded execution %s survives (%s)", e, x.Phase)
			case x.Phase == session.PhaseCancelled:
				t.Errorf("recorded execution %s was cancelled", e)
			case e != c.Current && !x.Phase.Settled():
				t.Errorf("execution %s is %s but not current", e, x.Phase)
			}
		}
	}
}
