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
