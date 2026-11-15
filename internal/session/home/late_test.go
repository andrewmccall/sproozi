package home

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

// passDeadline moves the coordinator's clock past execution's deadline, and
// lengthens the home's deadline so a new execution starts after it.
func passDeadline(t *testing.T, s *Service, execution string) {
	t.Helper()
	r, err := s.coordinator.Inspect(execution)
	if err != nil {
		t.Fatal(err)
	}
	if r.Deadline.IsZero() {
		t.Fatal("fixture execution has no deadline")
	}
	late := r.Deadline.Add(time.Second)
	s.coordinator.SetClock(func() time.Time { return late })
	s.duration = 2 * time.Hour
}

// wantDeadlineProblem checks a reply that names resume as the one next command.
func wantDeadlineProblem(t *testing.T, r Response, id string) {
	t.Helper()
	if !r.Rejected || !strings.Contains(r.Error, "reached its deadline before it started") ||
		!strings.Contains(r.Error, `"sproozi resume `+id+`"`) {
		t.Fatalf("late command replied %+v", r)
	}
}

// wantStoppedAndResumable checks that the session is stopped with its last
// checkpoint, that stop is idempotent, and that resume continues it from
// that checkpoint.
func wantStoppedAndResumable(t *testing.T, s *Service, id, digest, thread string) {
	t.Helper()
	status, err := s.Status(id)
	if err != nil || status.Phase != stageRetained || (digest != "") != (status.CheckpointInfo != nil) {
		t.Fatalf("status after the deadline: %+v %v", status, err)
	}
	if again, err := s.Stop(t.Context(), id, uuid.NewString()); err != nil || again.Phase != stageRetained {
		t.Fatalf("stop after the deadline: %+v %v", again, err)
	}
	resumed, err := s.Resume(t.Context(), id, uuid.NewString(), nil)
	if err != nil {
		t.Fatal(err)
	}
	active := resumed.Session
	if active == nil || active.Phase != session.PhaseActive || active.Active.Launch.CheckpointDigest != digest ||
		active.Active.Launch.ThreadID != thread {
		t.Fatalf("resume after the deadline: %+v", resumed)
	}
}

// Late commands that have no action of their own.
const (
	lateResumeAgain = "resume again"
	lateMoveLocal   = "move --local"
)

// A resume interrupted before its activation and retried after its deadline:
// whichever command comes next ends that start, after fencing what it may
// have prepared, and the session is stopped with the checkpoint it had, so
// resume continues it. A new resume does all of that itself.
func TestResumeInterruptedThenRetriedAfterItsDeadline(t *testing.T) {
	t.Parallel()
	for _, late := range []string{ActionResume, lateResumeAgain, ActionRecover, ActionStop, lateMoveLocal} {
		t.Run(late, func(t *testing.T) {
			t.Parallel()
			s, local, _, repo := testService(t)
			id := uuid.NewString()
			if _, err := s.Start(t.Context(), id, uuid.NewString(), repo, "", nil, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Stop(t.Context(), id, uuid.NewString()); err != nil {
				t.Fatal(err)
			}
			local.lostPrepare = true
			interrupted := uuid.NewString()
			if _, err := s.Resume(t.Context(), id, interrupted, nil); err == nil {
				t.Fatal("fixture did not interrupt the resume")
			}
			c, err := s.readCatalog(id)
			if err != nil {
				t.Fatal(err)
			}
			pending := c.Current
			passDeadline(t, s, pending)
			aborts := local.aborts
			var r Response
			switch late {
			case ActionResume:
				r = exchange(t, s, Request{Action: ActionResume, ID: id, OperationID: uuid.NewString()})
				if r.Error != "" || r.Status == nil || r.Status.Phase != string(session.PhaseActive) {
					t.Fatalf("late resume replied %+v", r)
				}
			case lateResumeAgain:
				r = exchange(t, s, Request{Action: ActionResume, ID: id, OperationID: interrupted})
			case ActionRecover:
				r = exchange(t, s, Request{Action: ActionRecover, ID: id})
			case ActionStop:
				r = exchange(t, s, Request{Action: ActionStop, ID: id, OperationID: uuid.NewString()})
				if r.Error != "" || r.Status == nil || r.Status.Phase != stageRetained {
					t.Fatalf("late stop replied %+v", r)
				}
			case lateMoveLocal:
				r = exchange(t, s, Request{Action: ActionMove, ID: id, OperationID: uuid.NewString(), Destination: LocalAlias})
			}
			if late != ActionResume && late != ActionStop {
				wantDeadlineProblem(t, r, id)
			}
			ended, err := s.coordinator.Inspect(pending)
			if err != nil || ended.Phase != session.PhaseIdle || !ended.Abandoned(interrupted) || local.aborts != aborts+1 {
				t.Fatalf("interrupted resume: %s abandoned %v aborts %d %v", ended.Phase, ended.Abandoned(interrupted),
					local.aborts-aborts, err)
			}
			if late == ActionResume {
				return
			}
			wantStoppedAndResumable(t, s, id, local.digest, testNativeUUID)
		})
	}
}

// "sproozi move ID --local" of a session on a cluster after its deadline
// stops the remote visit, keeps its checkpoint and says to resume, which
// brings the session home with a new deadline.
func TestMoveLocalAfterTheDeadlineStopsTheRemoteVisit(t *testing.T) {
	t.Parallel()
	s, local, remote, repo := testService(t)
	id := uuid.NewString()
	if _, err := s.Start(t.Context(), id, uuid.NewString(), repo, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if status, err := s.Move(t.Context(), id, uuid.NewString(), testRemote); err != nil || status.Placement != testRemote {
		t.Fatalf("move to the cluster: %+v %v", status, err)
	}
	passDeadline(t, s, id)
	prepares := local.prepares
	r := exchange(t, s, Request{Action: ActionMove, ID: id, OperationID: uuid.NewString(), Destination: LocalAlias})
	wantDeadlineProblem(t, r, id)
	if r.Status == nil || r.Status.Phase != stageRetained || r.Status.Placement != testRemote ||
		remote.stops != 1 || local.prepares != prepares {
		t.Fatalf("late move: %+v, remote stops %d, local prepares %d", r.Status, remote.stops, local.prepares-prepares)
	}
	wantStoppedAndResumable(t, s, id, remote.digest, testNativeUUID)
}

// A start whose first visit never ran, retried after its deadline, leaves the
// session stopped with the workspace it started from, and resume starts a
// new conversation from it.
func TestStartRetriedAfterItsDeadlineKeepsItsWorkspace(t *testing.T) {
	t.Parallel()
	s, local, _, repo := testService(t)
	id, operation := uuid.NewString(), uuid.NewString()
	local.lostPrepare = true
	if _, err := s.Start(t.Context(), id, operation, repo, "", nil, nil); err == nil {
		t.Fatal("fixture did not interrupt the start")
	}
	passDeadline(t, s, id)
	if _, err := s.Start(t.Context(), id, operation, repo, "", nil, nil); !errors.Is(err, session.ErrDeadlinePassed) {
		t.Fatalf("late start: %v", err)
	}
	status, err := s.Status(id)
	if err != nil || status.Phase != stageRetained || status.ExecutionID != id || local.aborts != 1 {
		t.Fatalf("status of a start its deadline ended: %+v %v", status, err)
	}
	// A replay of the start reports the same.
	if _, err = s.Start(t.Context(), id, operation, repo, "", nil, nil); !errors.Is(err, session.ErrDeadlinePassed) {
		t.Fatalf("replayed late start: %v", err)
	}
	wantStoppedAndResumable(t, s, id, "", "")
}
