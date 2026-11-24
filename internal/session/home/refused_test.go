package home

import (
	"errors"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

const testRefusal = "AgentRun session-0 failed before its sandbox started: RuntimeInvalid: image is required"

// wantRefusedProblem checks a reply that names the destination's refusal,
// not the deadline, and resume as the one next command.
func wantRefusedProblem(t *testing.T, r Response, id, reason string) {
	t.Helper()
	if !r.Rejected || !strings.Contains(r.Error, "session "+id+" can never start at its destination: "+reason) ||
		!strings.Contains(r.Error, "so it is stopped with its work kept") ||
		!strings.Contains(r.Error, `"sproozi resume `+id+`"`) || strings.Contains(r.Error, "deadline before it started") {
		t.Fatalf("refused move replied %+v", r)
	}
}

// "sproozi move ID --remote" of a session whose executor refuses the start
// for good, as one whose AgentRun failed before its sandbox started does,
// ends at once with the reason, before the deadline: the session is
// retained with the checkpoint its local stop committed, a replay of the
// move finds it retained without asking again, and resume continues it
// on this machine (ADR 0067).
func TestMoveRefusedByTheDestinationStopsTheSessionWithItsWork(t *testing.T) {
	t.Parallel()
	s, local, remote, repo := testService(t)
	id := uuid.NewString()
	if _, err := s.Start(t.Context(), id, uuid.NewString(), repo, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	remote.refuse = &session.StartRefused{Reason: testRefusal}
	operation := uuid.NewString()
	r := exchange(t, s, Request{Action: ActionMove, ID: id, OperationID: operation, Destination: testRemote})
	wantRefusedProblem(t, r, id, testRefusal)
	if r.Status == nil || r.Status.Phase != string(PhaseRetained) || r.Status.Placement != testLocal ||
		local.stops != 1 || remote.aborts != 1 {
		t.Fatalf("refused move: %+v, local stops %d", r.Status, local.stops)
	}
	// The session retired the execution: a replay of the move finds it
	// retained, names resume and asks the executor nothing.
	prepares := remote.prepares
	r = exchange(t, s, Request{Action: ActionMove, ID: id, OperationID: operation, Destination: testRemote})
	if !r.Rejected || !strings.Contains(r.Error, "is retained (stopped, with its work kept)") || remote.prepares != prepares {
		t.Fatalf("replayed refused move replied %+v", r)
	}
	if r = exchange(t, s, Request{Action: ActionRecover, ID: id}); !r.Rejected ||
		!strings.Contains(r.Error, `"sproozi resume `+id+`" continues it`) {
		t.Fatalf("recover after the refused move replied %+v", r)
	}
	wantStoppedAndResumable(t, s, id, local.digest, testNativeUUID)
}

// A session whose move was refused is stopped with its work kept, so cancel
// refuses it, naming resume, as for any stopped session; rm removes it.
func TestASessionWhoseMoveWasRefusedIsKeptUntilRemoved(t *testing.T) {
	t.Parallel()
	s, _, remote, repo := testService(t)
	id := uuid.NewString()
	if _, err := s.Start(t.Context(), id, uuid.NewString(), repo, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	remote.refuse = &session.StartRefused{Reason: testRefusal}
	if _, err := s.Move(t.Context(), id, uuid.NewString(), testRemote); !errors.Is(err, session.ErrStartRefused) {
		t.Fatalf("refused move: %v", err)
	}
	if r := exchange(t, s, Request{Action: ActionCancel, ID: id, OperationID: uuid.NewString()}); !r.Rejected ||
		!strings.Contains(r.Error, "is retained (stopped, with its work kept)") {
		t.Fatalf("cancel after the refused move replied %+v", r)
	}
	if r := exchange(t, s, Request{Action: ActionRemove, ID: id}); r.Error != "" {
		t.Fatalf("rm after the refused move replied %+v", r)
	}
	if _, err := s.Status(id); err == nil {
		t.Fatal("the removed session is still listed")
	}
}

// While the destination's refused preparation is not shown to have ended,
// the reply names the refusal and both ways on, and nothing is recorded.
func TestUnprovenRefusedMoveNamesRetryAndCancel(t *testing.T) {
	t.Parallel()
	s, _, remote, repo := testService(t)
	id := uuid.NewString()
	if _, err := s.Start(t.Context(), id, uuid.NewString(), repo, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	remote.refuse = &session.StartRefused{Reason: testRefusal}
	remote.unreachableAbort = true
	r := exchange(t, s, Request{Action: ActionMove, ID: id, OperationID: uuid.NewString(), Destination: testRemote})
	if !r.Rejected || !strings.Contains(r.Error, "session "+id+" can never start at its destination: "+testRefusal) ||
		!strings.Contains(r.Error, "but the placement has not shown that nothing runs") ||
		!strings.Contains(r.Error, `"sproozi cancel `+id+`"`) || strings.Contains(r.Error, "deadline") {
		t.Fatalf("unproven refused move replied %+v", r)
	}
	if status, err := s.Status(id); err != nil || status.Phase != string(session.PhasePreparing) {
		t.Fatalf("unproven refused move was recorded: %+v %v", status, err)
	}
}
