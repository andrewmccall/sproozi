package home

import (
	"errors"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
)

// An exited agent whose stop keeps failing binds at most one stop operation
// to its session however many sweeps see it, so the session's own stop and
// cancel are never refused for a full operation list.
func TestAnExitedAgentWhoseStopFailsBindsOneStop(t *testing.T) {
	t.Parallel()
	s, local, _, repo := testService(t)
	id := startedOn(t, s, repo, testLocal)
	before, err := s.readCatalog(id)
	if err != nil {
		t.Fatal(err)
	}
	local.exited = &session.Exit{Code: 2}
	local.stopErr = errors.New("capture failed")
	for range 5 {
		select {
		case <-s.sweepExits(t.Context()):
		case <-time.After(5 * time.Second):
			t.Fatal("sweep did not finish")
		}
	}
	after, err := s.readCatalog(id)
	if err != nil {
		t.Fatal(err)
	}
	if added := len(after.Operations) - len(before.Operations); added > 1 {
		t.Fatalf("five sweeps bound %d stop operations", added)
	}
	execution, err := s.coordinator.Inspect(id)
	if err != nil || execution.Phase == session.PhaseActive {
		t.Fatalf("the failed stop left the execution %s: %v", execution.Phase, err)
	}
}
