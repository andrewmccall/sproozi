package home

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// internalErrors are replies no command should reach: the table refuses
// every command before them.
var internalErrors = []string{"has no current execution", "still has a current execution"}

// requestFor is command on id with the fields it needs.
func requestFor(command, id string) Request {
	q := Request{Action: command, ID: id}
	switch command {
	case ActionMove:
		q.OperationID, q.Destination = uuid.NewString(), testRemote
	case ActionPull, ActionResume, ActionStop, ActionCancel:
		q.OperationID = uuid.NewString()
	}
	return q
}

// resumedSession starts a session, stops it and resumes it, so its current
// execution's ID differs from the session's.
func resumedSession(t *testing.T, s *Service, repo string) (string, string) {
	t.Helper()
	id := startedOn(t, s, repo, testLocal)
	if _, err := s.Stop(t.Context(), id, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	r, err := s.Resume(t.Context(), id, uuid.NewString(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return id, r.ExecutionID
}

// lateResume stops a started session and resumes it with its activation
// reply lost, so the resumed execution stays activating.
func lateResume(t *testing.T) (*Service, string, string) {
	s, local, _, repo := testService(t)
	id := startedOn(t, s, repo, testLocal)
	if _, err := s.Stop(t.Context(), id, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	local.lostActivate = true
	if _, err := s.Resume(t.Context(), id, uuid.NewString(), nil); err == nil {
		t.Fatal("a lost activation reply resumed the session")
	}
	c, err := s.readCatalog(id)
	if err != nil {
		t.Fatal(err)
	}
	return s, id, c.Current
}

// resumedFixtures bring a resumed session to every state its current
// execution can be in, returning the session and that execution.
var resumedFixtures = map[step]func(t *testing.T) (*Service, string, string){
	stepRetained: func(t *testing.T) (*Service, string, string) {
		s, local, _, repo := testService(t)
		id := startedOn(t, s, repo, testLocal)
		return s, id, interruptedStopOfAResume(t, s, local, id, nil)
	},
	stepStarting: lateResume,
	stepOverdue: func(t *testing.T) (*Service, string, string) {
		s, id, e := lateResume(t)
		passDeadline(t, s, e)
		return s, id, e
	},
	stepRunning: func(t *testing.T) (*Service, string, string) {
		s, _, _, repo := testService(t)
		id, e := resumedSession(t, s, repo)
		return s, id, e
	},
	stepStopping: func(t *testing.T) (*Service, string, string) {
		s, local, _, repo := testService(t)
		id, e := resumedSession(t, s, repo)
		local.lostStop = true
		if _, err := s.Stop(t.Context(), id, uuid.NewString()); err == nil {
			t.Fatal("a lost stop reply stopped the session")
		}
		return s, id, e
	},
	stepMoving: func(t *testing.T) (*Service, string, string) {
		s, local, _, repo := testService(t)
		id, e := resumedSession(t, s, repo)
		local.lostStop = true
		if _, err := s.Move(t.Context(), id, uuid.NewString(), testRemote); err == nil {
			t.Fatal("a lost stop reply moved the session")
		}
		return s, id, e
	},
	stepCancelling: func(t *testing.T) (*Service, string, string) {
		s, local, _, repo := testService(t)
		id, e := resumedSession(t, s, repo)
		local.unreachableAbort = true
		if _, err := s.Cancel(t.Context(), id, uuid.NewString()); err == nil {
			t.Fatal("an unreachable abort finished the cancellation")
		}
		return s, id, e
	},
	stepCancelled: func(t *testing.T) (*Service, string, string) {
		s, _, _, repo := testService(t)
		id, e := resumedSession(t, s, repo)
		if _, err := s.Cancel(t.Context(), id, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		return s, id, e
	},
}

// byIDOnly are commands whose execution-ID form differs from the session's by
// design: pull and resume act on a session, and rm of an execution its
// session records is refused in favour of the session.
var byIDOnly = []string{ActionPull, ActionResume, ActionRemove}

// Every command naming the current execution of a resumed session, in every
// state, gets the reply the same command naming the session gets, and none
// reaches an internal error. A command refused by the execution's ID changes
// nothing, so its session form runs on the same fixture; any other gets a
// fresh one.
func TestEveryCommandByExecutionIDMatchesTheSession(t *testing.T) {
	t.Parallel()
	for row, fixture := range resumedFixtures {
		t.Run(string(row), func(t *testing.T) {
			t.Parallel()
			var s *Service
			var id, e string
			for _, command := range commands {
				if s == nil {
					s, id, e = fixture(t)
					if before := stepOf(t, s, id); before != row {
						t.Fatalf("the fixture reached %s, not %s", before, row)
					}
				}
				byExecution := exchange(t, s, requestFor(command, e))
				byExecution.Error = placeholders(byExecution.Error, id, e)
				if !byExecution.Rejected || rowAfter(t, s, id) != row {
					s = nil
				}
				if s == nil {
					s, id, e = fixture(t)
				}
				bySession := exchange(t, s, requestFor(command, id))
				bySession.Error = placeholders(bySession.Error, id, e)
				if !bySession.Rejected || rowAfter(t, s, id) != row {
					s = nil
				}
				for _, r := range []Response{byExecution, bySession} {
					for _, internal := range internalErrors {
						if strings.Contains(r.Error, internal) {
							t.Errorf("%s reached %q", command, r.Error)
						}
					}
				}
				if slices.Contains(byIDOnly, command) {
					continue
				}
				if byExecution.Error != bySession.Error || byExecution.Rejected != bySession.Rejected {
					t.Errorf("%s by execution replied rejected=%v %q, by session rejected=%v %q", command,
						byExecution.Rejected, byExecution.Error, bySession.Rejected, bySession.Error)
				}
			}
		})
	}
}

// placeholders names the fixture's session and execution by role, so
// replies from two fixtures compare.
func placeholders(reply, id, execution string) string {
	return strings.NewReplacer(id, "ID", execution, "EXECUTION").Replace(reply)
}
