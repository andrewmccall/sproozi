package home

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/work"
	"github.com/google/uuid"
)

// fillCatalogue fills the catalogue directory to its entry limit, so the next
// catalogue write fails as it does once 512 entries are kept. The returned
// function empties it again.
func fillCatalogue(t *testing.T, s *Service) func() {
	t.Helper()
	dir, err := s.catalogDir()
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, 512)
	for i := range 512 {
		name := filepath.Join(dir, fmt.Sprintf("filler-%d.tmp", i))
		if err = os.WriteFile(name, nil, 0600); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	return func() {
		for _, name := range names {
			if err := os.Remove(name); err != nil {
				t.Error(err)
			}
		}
	}
}

// A stop whose catalogue write failed after the execution's journal recorded
// it leaves the journal idle and the catalogue naming it as the current
// execution. The session reads as stopped, with the stop's checkpoint: it is
// listed as retained, refuses commands that need a running session instead
// of cancelling or moving it, resumes from that checkpoint and can be removed.
func TestAStopWhoseCatalogueWriteFailedReadsAsStopped(t *testing.T) {
	t.Parallel()
	s, local, remote, repo := testService(t)
	id := startedOn(t, s, repo, testLocal)
	release := func() {}
	local.onStop = func() { release = fillCatalogue(t, s) }
	if _, err := s.Stop(t.Context(), id, uuid.NewString()); err == nil {
		t.Fatal("stop saved its catalogue entry past the catalogue's limit")
	}
	release()
	if execution, err := s.coordinator.Inspect(id); err != nil || execution.Phase != session.PhaseIdle {
		t.Fatalf("the stop did not reach the journal: %+v %v", execution.Phase, err)
	}
	if c, err := s.readCatalog(id); err != nil || c.Current != id {
		t.Fatalf("the catalogue recorded the stop: %+v %v", c, err)
	}
	status, err := s.Status(id)
	if err != nil || status.Phase != string(PhaseRetained) || status.InitialStage != status.Phase ||
		status.CheckpointInfo == nil || status.ThreadID != testNativeUUID || status.Ownership != ownershipHome {
		t.Fatalf("status of the stopped session: %+v %v", status, err)
	}
	listed, err := s.Sessions(work.SessionQuery{})
	if err != nil || len(listed) != 1 || listed[0].Phase != string(PhaseRetained) {
		t.Fatalf("listing of the stopped session: %+v %v", listed, err)
	}
	for _, q := range []Request{
		{Action: ActionCancel, ID: id, OperationID: uuid.NewString()},
		{Action: ActionMove, ID: id, OperationID: uuid.NewString(), Destination: testRemote},
		{Action: ActionRecover, ID: id},
		{Action: ActionAttach, ID: id},
	} {
		if r := exchange(t, s, q); !r.Rejected || !strings.Contains(r.Error, "is retained") {
			t.Errorf("%s of the stopped session replied %+v", q.Action, r)
		}
	}
	if execution, err := s.coordinator.Inspect(id); err != nil || execution.Phase != session.PhaseIdle ||
		remote.prepares != 0 {
		t.Fatalf("a refused command changed the stopped session: %+v %v", execution.Phase, err)
	}
	resumed, err := s.Resume(t.Context(), id, uuid.NewString(), nil)
	if err != nil || resumed.Session == nil || resumed.Session.Active == nil ||
		resumed.Session.Active.Launch.CheckpointDigest != local.digest {
		t.Fatalf("resume of the stopped session: %+v %v", resumed, err)
	}
	if _, err = s.Stop(t.Context(), id, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if r := exchange(t, s, Request{Action: ActionRemove, ID: id}); r.Error != "" {
		t.Fatalf("remove replied %+v", r)
	}
}

// interruptedResume stops a started session and resumes it while the
// catalogue is full, so the resume retains its new execution but cannot
// record it in the session's catalogue entry.
func interruptedResume(t *testing.T, s *Service, id string) string {
	t.Helper()
	if _, err := s.Stop(t.Context(), id, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	release := fillCatalogue(t, s)
	operation := uuid.NewString()
	if _, err := s.Resume(t.Context(), id, operation, nil); err == nil {
		t.Fatal("resume recorded its execution past the catalogue's limit")
	}
	release()
	ids, err := s.coordinator.List()
	if err != nil || len(ids) != 2 {
		t.Fatalf("the interrupted resume retained no execution: %v %v", ids, err)
	}
	return operation
}

// An interrupted resume's execution, which its session never recorded, is
// removed with the session.
func TestRemoveTakesAnInterruptedResumesExecution(t *testing.T) {
	t.Parallel()
	s, local, _, repo := testService(t)
	id := startedOn(t, s, repo, testLocal)
	interruptedResume(t, s, id)
	if local.prepares != 0 {
		t.Fatalf("the interrupted resume prepared its execution: %d", local.prepares)
	}
	if r := exchange(t, s, Request{Action: ActionRemove, ID: id}); r.Error != "" {
		t.Fatalf("remove replied %+v", r)
	}
	if ids, err := s.coordinator.List(); err != nil || len(ids) != 0 {
		t.Fatalf("remove left executions: %v %v", ids, err)
	}
	if count := countFiles(t, filepath.Join(s.config.Directory, "journal")); count != 0 {
		t.Fatalf("remove left %d journal files", count)
	}
}

// The session's next resume releases the retention slot an interrupted
// resume's execution held, whether it repeats the interrupted operation or
// starts another, and never starts that execution.
func TestResumeReleasesAnInterruptedResumesExecution(t *testing.T) {
	t.Parallel()
	for _, repeat := range []bool{false, true} {
		t.Run(fmt.Sprintf("repeat=%v", repeat), func(t *testing.T) {
			t.Parallel()
			s, local, _, repo := testService(t)
			id := startedOn(t, s, repo, testLocal)
			operation := interruptedResume(t, s, id)
			if !repeat {
				operation = uuid.NewString()
			}
			resumed, err := s.Resume(t.Context(), id, operation, nil)
			if err != nil || resumed.Phase != string(session.PhaseActive) {
				t.Fatalf("resume after the interruption: %+v %v", resumed, err)
			}
			ids, err := s.coordinator.List()
			if err != nil || len(ids) != 2 {
				t.Fatalf("executions after the resume: %v %v", ids, err)
			}
			c, err := s.readCatalog(id)
			if err != nil || len(c.Executions) != 2 || c.Current != resumed.ExecutionID {
				t.Fatalf("catalogue after the resume: %+v %v", c, err)
			}
			if local.prepares != 1 || local.activations != 2 {
				t.Fatalf("prepares %d, activations %d", local.prepares, local.activations)
			}
		})
	}
}
