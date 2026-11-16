package home

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

// threeExecutions starts a session, stops it, resumes and stops it, and
// resumes it again, returning the session and its past, recorded, settled
// second execution.
func threeExecutions(t *testing.T, s *Service, repo string) (string, string) {
	t.Helper()
	id := startedOn(t, s, repo, testLocal)
	if _, err := s.Stop(t.Context(), id, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	second, err := s.Resume(t.Context(), id, uuid.NewString(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Stop(t.Context(), id, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Resume(t.Context(), id, uuid.NewString(), nil); err != nil {
		t.Fatal(err)
	}
	return id, second.ExecutionID
}

// A past execution the session records is reached only through the
// session: rm and cancel of its own ID are refused and leave it as it was,
// since the session may still restore from it.
func TestARecordedPastExecutionIsReachedOnlyThroughItsSession(t *testing.T) {
	t.Parallel()
	s, _, _, repo := testService(t)
	id, past := threeExecutions(t, s, repo)
	want := "home: " + past + " belongs to session " + id + "; remove that session"
	if r := exchange(t, s, Request{Action: ActionRemove, ID: past}); !r.Rejected || r.Error != want {
		t.Fatalf("rm of the past execution replied %+v, want %q", r, want)
	}
	r := exchange(t, s, Request{Action: ActionCancel, ID: past, OperationID: uuid.NewString()})
	if r.Error != "home: execution is no longer the current session writer" {
		t.Fatalf("cancel of the past execution replied %+v", r)
	}
	if x, err := s.coordinator.Inspect(past); err != nil || x.Phase != session.PhaseIdle || x.CancelOperation != "" {
		t.Fatalf("the past execution changed: %s %q %v", x.Phase, x.CancelOperation, err)
	}
}

// An execution its session never recorded is reached by cancel and rm of its
// ID only: stop, move, recover and attach are refused and start nothing, and
// rm of it before it settles names what to do.
func TestOnlyCancelAndRemoveReachAnUnrecordedExecution(t *testing.T) {
	t.Parallel()
	s, local, remote, repo := testService(t)
	id := startedOn(t, s, repo, testLocal)
	interruptedResume(t, s, id)
	orphan := orphanOf(t, s, id)
	for _, command := range []string{ActionStop, ActionMove, ActionRecover, ActionAttach} {
		q := Request{Action: command, ID: orphan}
		switch command {
		case ActionMove:
			q.OperationID, q.Destination = uuid.NewString(), testRemote
		case ActionStop:
			q.OperationID = uuid.NewString()
		}
		if r := exchange(t, s, q); r.Error != "home: execution is no longer the current session writer" {
			t.Errorf("%s of the unrecorded execution replied %+v", command, r)
		}
	}
	if x, err := s.coordinator.Inspect(orphan); err != nil || x.Phase != session.PhasePreparing ||
		x.CancelOperation != "" || local.prepares != 0 || remote.prepares != 0 {
		t.Fatalf("a refused command changed the unrecorded execution: %s %v", x.Phase, err)
	}
	want := unrecordedProblem(id, orphan).Error()
	if r := exchange(t, s, Request{Action: ActionRemove, ID: orphan}); !r.Rejected || r.Error != want {
		t.Fatalf("rm of the unsettled execution replied %+v, want %q", r, want)
	}
}

// A cancel of an unrecorded execution waits for its session's lock, which a
// resume holds from retaining its new execution until recording it, and is
// refused once the resume has recorded it.
func TestCancelOfAnUnrecordedExecutionHoldsItsSessionsLock(t *testing.T) {
	t.Parallel()
	s, _, _, repo := testService(t)
	id := startedOn(t, s, repo, testLocal)
	interruptedResume(t, s, id)
	orphan := orphanOf(t, s, id)
	unlock, err := s.lockSession(id)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Cancel(t.Context(), orphan, uuid.NewString())
		done <- err
	}()
	select {
	case err = <-done:
		unlock()
		t.Fatalf("cancel did not wait for the session's lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	// As a resume does under the lock: record the execution as current.
	c, err := s.readCatalog(id)
	if err != nil {
		t.Fatal(err)
	}
	c.Current, c.Executions = orphan, append(c.Executions, orphan)
	if err = s.saveCatalog(c); err != nil {
		t.Fatal(err)
	}
	unlock()
	if err = <-done; err == nil || !strings.Contains(err.Error(), "now records execution "+orphan) {
		t.Fatalf("cancel after the resume recorded the execution: %v", err)
	}
	if x, err := s.coordinator.Inspect(orphan); err != nil || x.CancelOperation != "" {
		t.Fatalf("the recorded execution was cancelled: %q %v", x.CancelOperation, err)
	}
}

// A journal the service cannot read is logged once with its file, not on
// every command that looks at it, and not again after an exit sweep.
func TestAnUnreadableJournalIsLoggedOnce(t *testing.T) {
	t.Parallel()
	s, _, _, repo := testService(t)
	var log strings.Builder
	s.log = &log
	ids := make([]string, 0, 2)
	for range 2 {
		id := startedOn(t, s, repo, testLocal)
		if _, err := s.Stop(t.Context(), id, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	unreadable := uuid.NewString()
	bad := filepath.Join(s.config.Directory, "journal", unreadable+".json")
	if err := os.WriteFile(bad, []byte(`{"version":99,"futureField":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if r := exchange(t, s, Request{Action: ActionRemove, ID: ids[0]}); r.Error != "" {
		t.Fatalf("rm replied %+v", r)
	}
	select {
	case <-s.sweepExits(t.Context()):
	case <-time.After(5 * time.Second):
		t.Fatal("sweep did not finish")
	}
	if r := exchange(t, s, Request{Action: ActionRemove, ID: ids[1]}); r.Error != "" {
		t.Fatalf("rm replied %+v", r)
	}
	line := "Failed to read execution " + unreadable + "'s journal " + bad + ", so kept it: "
	if got := log.String(); strings.Count(got, line) != 1 {
		t.Fatalf("log: %q", got)
	}
}
