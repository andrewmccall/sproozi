package home

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

// derivedStates is one state for every catalogue entry and journal record
// shape derive distinguishes, named by what it shows.
func derivedStates(t *testing.T) map[string]state {
	t.Helper()
	failed := &session.Exit{Code: 2}
	late := &session.Exit{Code: 1, Deadline: true}
	claimed := func(exit *session.Exit) *localSession {
		return &localSession{Claimed: true, Committed: session.Stopped{Exit: exit}}
	}
	record := func(phase session.Phase, mutate ...func(*session.Record)) *session.Record {
		r := &session.Record{Phase: phase, Operations: []session.Operation{{ID: uuid.NewString()}}}
		for _, m := range mutate {
			m(r)
		}
		return r
	}
	cancelRequested := func(r *session.Record) { r.CancelOperation = uuid.NewString() }
	expired := func(r *session.Record) { r.Deadline = time.Unix(1, 0) }
	finalizing := func(r *session.Record) {
		r.PendingOperation = r.Operations[0].ID
		r.Operations[0].Finalize = true
	}
	moving := func(r *session.Record) { r.PendingOperation = r.Operations[0].ID }
	visit := func(exit *session.Exit) func(*session.Record) {
		return func(r *session.Record) { r.Visits = []session.Visit{{Exit: exit}} }
	}
	past := func(r session.Record) bool { return !r.Deadline.IsZero() && r.Deadline.Before(time.Now()) }
	cases := map[string]struct {
		c       *localSession
		current *session.Record
	}{
		"unclaimed":                       {c: &localSession{}},
		"stopped":                         {c: claimed(nil)},
		"stopped after a failure":         {c: claimed(failed)},
		"stopped at the deadline":         {c: claimed(late)},
		"idle current":                    {c: claimed(nil), current: record(session.PhaseIdle, visit(nil))},
		"idle current after a failure":    {c: claimed(nil), current: record(session.PhaseIdle, visit(failed))},
		"idle current, no visit, failed":  {c: claimed(failed), current: record(session.PhaseIdle)},
		"idle execution with no session":  {current: record(session.PhaseIdle)},
		"preparing":                       {c: claimed(nil), current: record(session.PhasePreparing)},
		"preparing past the deadline":     {c: claimed(nil), current: record(session.PhasePreparing, expired)},
		"activating":                      {c: claimed(nil), current: record(session.PhaseActivating)},
		"activating past the deadline":    {c: claimed(nil), current: record(session.PhaseActivating, expired)},
		"active writer":                   {c: claimed(nil), current: record(session.PhaseActive)},
		"stopping":                        {c: claimed(nil), current: record(session.PhaseStopping, finalizing)},
		"stopping to move":                {c: claimed(nil), current: record(session.PhaseStopping, moving)},
		"unknown-stop":                    {c: claimed(nil), current: record(session.PhaseUnknownStop, finalizing)},
		"unknown-stop, cancel requested":  {c: claimed(nil), current: record(session.PhaseUnknownStop, cancelRequested)},
		"unknown-usage":                   {c: claimed(nil), current: record(session.PhaseUnknownUsage, moving)},
		"unknown-usage, cancel requested": {c: claimed(nil), current: record(session.PhaseUnknownUsage, cancelRequested)},
		"cancelling":                      {c: claimed(nil), current: record(session.PhaseCancelling, cancelRequested)},
		"cancelled":                       {c: claimed(nil), current: record(session.PhaseCancelled, cancelRequested)},
	}
	states := map[string]state{}
	for name, tc := range cases {
		states[name] = derive(tc.c, tc.current, past)
	}
	return states
}

// derive reports one phase from the closed vocabulary for every shape, never
// idle, and every phase of the vocabulary is reachable.
func TestDeriveReportsOnePhaseForEveryShape(t *testing.T) {
	t.Parallel()
	want := map[string]Phase{
		"unclaimed":                       PhaseImporting,
		"stopped":                         PhaseRetained,
		"stopped after a failure":         PhaseFailed,
		"stopped at the deadline":         PhaseRetained,
		"idle current":                    PhaseRetained,
		"idle current after a failure":    PhaseFailed,
		"idle current, no visit, failed":  PhaseFailed,
		"idle execution with no session":  PhaseRetained,
		"preparing":                       Phase(session.PhasePreparing),
		"preparing past the deadline":     Phase(session.PhasePreparing),
		"activating":                      Phase(session.PhaseActivating),
		"activating past the deadline":    Phase(session.PhaseActivating),
		"active writer":                   Phase(session.PhaseActive),
		"stopping":                        Phase(session.PhaseStopping),
		"stopping to move":                Phase(session.PhaseStopping),
		"unknown-stop":                    Phase(session.PhaseUnknownStop),
		"unknown-stop, cancel requested":  Phase(session.PhaseUnknownStop),
		"unknown-usage":                   Phase(session.PhaseUnknownUsage),
		"unknown-usage, cancel requested": Phase(session.PhaseUnknownUsage),
		"cancelling":                      Phase(session.PhaseCancelling),
		"cancelled":                       Phase(session.PhaseCancelled),
	}
	reached := map[Phase]bool{}
	for name, st := range derivedStates(t) {
		if st.phase != want[name] {
			t.Errorf("%s: derived %q, want %q", name, st.phase, want[name])
		}
		if !slices.Contains(phases, st.phase) {
			t.Errorf("%s: derived %q, outside the vocabulary", name, st.phase)
		}
		reached[st.phase] = true
	}
	for _, p := range phases {
		if !reached[p] {
			t.Errorf("phase %q is never derived", p)
		}
	}
}

// commands is every session command whose refusal depends on the state.
var commands = []string{ActionPull, ActionResume, ActionAttach, ActionStop, ActionMove, ActionRecover, ActionCancel,
	ActionRemove}

// named is the commands a problem's advice names, as Action words.
func named(text string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`"sproozi (\w+)`).FindAllStringSubmatch(text, -1) {
		command := m[1]
		if command == "rm" {
			command = ActionRemove
		}
		if !slices.Contains(out, command) {
			out = append(out, command)
		}
	}
	return out
}

// deadlineClock is where a fixture's execution deadline stands on the
// coordinator's clock.
type deadlineClock string

const (
	clockAhead  deadlineClock = "ahead"
	clockPassed deadlineClock = "passed"
	clockNone   deadlineClock = "none"
)

// rowFixture brings a new service's session to one row of the next-steps
// table, through the service's own commands, under a clock. The exit it
// returns removes the cause the fixture used to hold the session there, an
// unreachable placement or a denied claim, and gives the request that then
// leaves the row.
type rowFixture struct {
	// clocks are the clocks the row can be reached under. stateFixtures
	// uses the first.
	clocks []deadlineClock
	build  func(t *testing.T, c deadlineClock) (s *Service, id string, exit func() Request)
}

// fixtureService is a new service whose executions get a deadline an hour
// ahead, or none under clockNone.
func fixtureService(t *testing.T, c deadlineClock) (*Service, *placementFixture, string) {
	t.Helper()
	s, local, _, repo := testService(t)
	if c == clockNone {
		s.duration = 0
	}
	return s, local, repo
}

// started is a session running on this machine under c.
func started(t *testing.T, c deadlineClock) (*Service, *placementFixture, string) {
	t.Helper()
	s, local, repo := fixtureService(t, c)
	return s, local, startedOn(t, s, repo, testLocal)
}

// passIfLate moves the coordinator's clock past every deadline the fixture
// set when c is clockPassed, as a restart after the deadline would. A new
// execution's deadline still lies ahead of it.
func passIfLate(t *testing.T, s *Service, c deadlineClock) {
	t.Helper()
	if c != clockPassed {
		return
	}
	late := time.Now().Add(s.duration + time.Minute)
	reopenWithClock(t, s, func() time.Time { return late })
	s.duration *= 2
}

// command is a request for action on session id, with a new operation for
// the actions that take one.
func command(action, id string) Request {
	q := Request{Action: action, ID: id}
	if slices.Contains([]string{ActionPull, ActionResume, ActionStop, ActionCancel}, action) {
		q.OperationID = uuid.NewString()
	}
	return q
}

// leaveWith is an exit that needs no cause removed.
func leaveWith(action, id string) func() Request {
	return func() Request { return command(action, id) }
}

// stoppedFixture is a session started under c and stopped, by an agent that
// exited with exit when it is non-nil.
func stoppedFixture(exit *session.Exit) func(*testing.T, deadlineClock) (*Service, string, func() Request) {
	return func(t *testing.T, c deadlineClock) (*Service, string, func() Request) {
		s, local, id := started(t, c)
		local.exited = exit
		if _, err := s.Stop(t.Context(), id, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		passIfLate(t, s, c)
		return s, id, leaveWith(ActionResume, id)
	}
}

// lostStopFixture is a session started under c whose stop, or move when move
// is set, lost the placement's reply, so it stays stopping.
func lostStopFixture(move bool, exit string) func(*testing.T, deadlineClock) (*Service, string, func() Request) {
	return func(t *testing.T, c deadlineClock) (*Service, string, func() Request) {
		s, local, id := started(t, c)
		local.lostStop = true
		var err error
		if move {
			_, err = s.Move(t.Context(), id, uuid.NewString(), testRemote)
		} else {
			_, err = s.Stop(t.Context(), id, uuid.NewString())
		}
		if err == nil {
			t.Fatal("a lost stop reply finished the stop")
		}
		passIfLate(t, s, c)
		return s, id, leaveWith(exit, id)
	}
}

var everyClock = []deadlineClock{clockAhead, clockPassed, clockNone}

// rowFixtures has one fixture for each row of the next-steps table.
var rowFixtures = map[step]rowFixture{
	// The pulled execution's deadline passed an hour ago, or it has none.
	stepImporting: {clocks: []deadlineClock{clockPassed, clockNone},
		build: func(t *testing.T, c deadlineClock) (*Service, string, func() Request) {
			s, _, source := importedFixture(t)
			if c == clockNone {
				source.export.Stopped.Launch.Deadline = time.Time{}
				source.export.Stopped.Receipt.Launch.Deadline = time.Time{}
			}
			source.denyClaim = true
			if _, err := s.Pull(t.Context(), source.export.ID, source.export.OperationID, testRemote); err == nil {
				t.Fatal("a refused claim finished the pull")
			}
			return s, source.export.ID, func() Request {
				source.denyClaim = false
				return Request{Action: ActionPull, ID: source.export.ID, OperationID: source.export.OperationID}
			}
		}},
	stepRetained: {clocks: everyClock, build: stoppedFixture(nil)},
	stepFailed:   {clocks: everyClock, build: stoppedFixture(&session.Exit{Code: 2})},
	// Past its deadline a starting session is overdue.
	stepStarting: {clocks: []deadlineClock{clockAhead, clockNone},
		build: func(t *testing.T, c deadlineClock) (*Service, string, func() Request) {
			s, id := startingFixture(t, c)
			return s, id, leaveWith(ActionRecover, id)
		}},
	// Without a deadline nothing is overdue.
	stepOverdue: {clocks: []deadlineClock{clockPassed},
		build: func(t *testing.T, _ deadlineClock) (*Service, string, func() Request) {
			s, id := startingFixture(t, clockAhead)
			passDeadline(t, s, id)
			return s, id, leaveWith(ActionStop, id)
		}},
	stepRunning: {clocks: everyClock, build: func(t *testing.T, c deadlineClock) (*Service, string, func() Request) {
		s, _, id := started(t, c)
		passIfLate(t, s, c)
		return s, id, leaveWith(ActionStop, id)
	}},
	stepStopping: {clocks: everyClock, build: lostStopFixture(false, ActionStop)},
	stepMoving:   {clocks: everyClock, build: lostStopFixture(true, ActionRecover)},
	stepCancelling: {clocks: everyClock, build: func(t *testing.T, c deadlineClock) (*Service, string, func() Request) {
		s, local, id := started(t, c)
		local.unreachableAbort = true
		if _, err := s.Cancel(t.Context(), id, uuid.NewString()); err == nil {
			t.Fatal("an unreachable abort finished the cancellation")
		}
		passIfLate(t, s, c)
		return s, id, func() Request {
			local.unreachableAbort = false
			return command(ActionRecover, id)
		}
	}},
	stepCancelled: {clocks: everyClock, build: func(t *testing.T, c deadlineClock) (*Service, string, func() Request) {
		s, _, id := started(t, c)
		if _, err := s.Cancel(t.Context(), id, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		passIfLate(t, s, c)
		return s, id, leaveWith(ActionRemove, id)
	}},
}

// stateFixtures brings a new service's session to each row of the
// next-steps table, with the placement failing where a state needs it,
// under the row's first clock.
var stateFixtures = func() map[step]func(t *testing.T) (*Service, string) {
	out := map[step]func(t *testing.T) (*Service, string){}
	for row, f := range rowFixtures {
		out[row] = func(t *testing.T) (*Service, string) {
			s, id, _ := f.build(t, f.clocks[0])
			return s, id
		}
	}
	return out
}()

// startingFixture starts a session under c whose activation reply is lost,
// so it stays activating.
func startingFixture(t *testing.T, c deadlineClock) (*Service, string) {
	s, local, repo := fixtureService(t, c)
	id := uuid.NewString()
	local.lostActivate = true
	if _, err := s.Start(t.Context(), id, uuid.NewString(), repo, "", nil, nil); err == nil {
		t.Fatal("a lost activation reply started the session")
	}
	return s, id
}

// stepOf is the table row session id is in.
func stepOf(t *testing.T, s *Service, id string) step {
	t.Helper()
	c, err := s.readCatalog(id)
	if err != nil {
		t.Fatal(err)
	}
	st, _, err := s.sessionState(c)
	if err != nil {
		t.Fatal(err)
	}
	return st.step()
}

// Each row's reply, as the service gives it, with ID for the session.
const (
	replyImporting = `home: session ID is still being pulled; finish with "sproozi pull ID" first`
	replyRetained  = `home: session ID is retained (stopped, with its work kept); "sproozi resume ID" continues it`
	replyFailed    = `home: session ID stopped because its agent exited with code 2; see "sproozi status ID", ` +
		`then "sproozi resume ID" to continue it or "sproozi rm ID" to remove it`
	replyStarting = `home: session ID is still starting or moving; run "sproozi recover ID" to finish that ` +
		`first, or "sproozi cancel ID" to stop it now`
	replyOverdue = `home: session ID reached its deadline before it started; "sproozi resume ID" ends that ` +
		`start and continues the session with a new deadline, or, if the placement cannot show that nothing ` +
		`runs there, "sproozi cancel ID" records the cancellation and finishes it once the placement can show that`
	replyRunning = `home: session ID is running; attach to it with "sproozi attach ID", stop it with ` +
		`"sproozi stop ID", or force-stop it with "sproozi cancel ID"`
	replyStopping = `home: session ID is still stopping; run "sproozi stop ID" to finish, or ` +
		`"sproozi cancel ID" to force-stop it`
	replyMoving = `home: session ID is still stopping to move; run "sproozi recover ID" to finish the move, ` +
		`or "sproozi cancel ID" to force-stop it`
	replyCancelling = `home: session ID is being cancelled; run "sproozi recover ID" to finish the ` +
		`cancellation, then "sproozi rm ID" to remove it`
	replyCancelled = `home: session ID was cancelled, so it cannot be stopped or resumed; remove it with ` +
		`"sproozi rm ID", and start new work with "sproozi run"`

	// Refusals that are not the table's: the execution journal's, mapped
	// by needsRecovery, and a move with no conversation to move.
	replyNoConversation = `home: ID has no saved conversation yet; finish a turn before moving it to remote, ` +
		`or continue it at home`
	replyStartUnfinished = `session: previous start requires recovery; run "sproozi recover ID" to finish it, ` +
		`or "sproozi cancel ID" to stop it now`
	replyMoveUnfinished = `session: previous move requires recovery; run "sproozi recover ID" to finish it, ` +
		`or "sproozi cancel ID" to stop it now`
	replyStopPending = `session ID is still stopping; run "sproozi stop ID" to keep waiting, or ` +
		`"sproozi cancel ID" to force-stop it`
	replyCancelUnfinished = `session: cancellation unfinished; run "sproozi recover ID" to finish the cancellation`
)

// outcome is what a command does: the reply, "" when it succeeds, whether
// it is a rejection, and the row the session is in afterwards, "" when it is
// removed.
type outcome struct {
	reply    string
	rejected bool
	after    step
}

func refused(reply string, row step) outcome {
	return outcome{reply: reply, rejected: true, after: row}
}
func done(after step) outcome { return outcome{after: after} }

// failedWith is a command carried out that failed for a reason outside the
// session's state, here the fixture's unreachable placement.
func failedWith(reply string, after step) outcome { return outcome{reply: reply, after: after} }

// outcomes is what every command does in every row, written out here rather
// than taken from nextSteps.
var outcomes = map[step]map[string]outcome{
	stepImporting: {
		ActionPull: failedWith("claim denied", stepImporting), ActionResume: refused(replyImporting, stepImporting),
		ActionAttach: refused(replyImporting, stepImporting), ActionStop: refused(replyImporting, stepImporting),
		ActionMove: refused(replyImporting, stepImporting), ActionRecover: refused(replyImporting, stepImporting),
		ActionCancel: refused(replyImporting, stepImporting), ActionRemove: refused(replyImporting, stepImporting),
	},
	stepRetained: {
		ActionPull: done(stepRetained), ActionResume: done(stepRunning),
		ActionAttach: refused(replyRetained, stepRetained), ActionStop: done(stepRetained),
		ActionMove: refused(replyRetained, stepRetained), ActionRecover: refused(replyRetained, stepRetained),
		ActionCancel: refused(replyRetained, stepRetained), ActionRemove: done(""),
	},
	stepFailed: {
		ActionPull: done(stepFailed), ActionResume: done(stepRunning),
		ActionAttach: refused(replyFailed, stepFailed), ActionStop: done(stepFailed),
		ActionMove: refused(replyFailed, stepFailed), ActionRecover: refused(replyFailed, stepFailed),
		ActionCancel: refused(replyFailed, stepFailed), ActionRemove: done(""),
	},
	stepStarting: {
		ActionPull: done(stepStarting), ActionResume: refused(replyStarting, stepStarting),
		ActionAttach: refused(replyStarting, stepStarting), ActionStop: refused(replyStartUnfinished, stepStarting),
		ActionMove: refused(replyNoConversation, stepStarting), ActionRecover: done(stepRunning),
		ActionCancel: done(stepCancelled), ActionRemove: refused(replyStarting, stepStarting),
	},
	// The fixture's destination did become active, so the commands that end
	// a late start record it running instead (ADR 0060), and resume then
	// refuses it as running.
	stepOverdue: {
		ActionPull: done(stepOverdue), ActionResume: refused(replyRunning, stepRunning),
		ActionAttach: refused(replyOverdue, stepOverdue), ActionStop: done(stepRetained),
		ActionMove: refused(replyNoConversation, stepOverdue), ActionRecover: done(stepRunning),
		ActionCancel: done(stepCancelled), ActionRemove: refused(replyOverdue, stepOverdue),
	},
	stepRunning: {
		ActionPull: done(stepRunning), ActionResume: refused(replyRunning, stepRunning),
		ActionAttach: done(stepRunning), ActionStop: done(stepRetained),
		ActionMove: done(stepRunning), ActionRecover: done(stepRunning),
		ActionCancel: done(stepCancelled), ActionRemove: refused(replyRunning, stepRunning),
	},
	stepStopping: {
		ActionPull: done(stepStopping), ActionResume: refused(replyStopping, stepStopping),
		ActionAttach: refused(replyStopping, stepStopping), ActionStop: done(stepRetained),
		ActionMove: refused(replyStopPending, stepStopping), ActionRecover: done(stepRetained),
		ActionCancel: done(stepCancelled), ActionRemove: refused(replyStopping, stepStopping),
	},
	stepMoving: {
		ActionPull: done(stepMoving), ActionResume: refused(replyMoving, stepMoving),
		ActionAttach: refused(replyMoving, stepMoving), ActionStop: refused(replyMoveUnfinished, stepMoving),
		ActionMove: refused(replyMoveUnfinished, stepMoving), ActionRecover: done(stepRunning),
		ActionCancel: done(stepCancelled), ActionRemove: refused(replyMoving, stepMoving),
	},
	stepCancelling: {
		ActionPull: done(stepCancelling), ActionResume: refused(replyCancelling, stepCancelling),
		ActionAttach: refused(replyCancelling, stepCancelling), ActionStop: refused(replyCancelUnfinished, stepCancelling),
		ActionMove:    refused(replyCancelUnfinished, stepCancelling),
		ActionRecover: failedWith("docker unreachable", stepCancelling),
		ActionCancel:  failedWith("docker unreachable", stepCancelling), ActionRemove: refused(replyCancelling, stepCancelling),
	},
	stepCancelled: {
		ActionPull: done(stepCancelled), ActionResume: refused(replyCancelled, stepCancelled),
		ActionAttach: refused(replyCancelled, stepCancelled), ActionStop: refused(replyCancelled, stepCancelled),
		ActionMove: refused(replyNoConversation, stepCancelled), ActionRecover: done(stepCancelled),
		ActionCancel: done(stepCancelled), ActionRemove: done(""),
	},
}

// rowReplies are the table's own replies, by row.
var rowReplies = map[step]string{stepImporting: replyImporting, stepRetained: replyRetained,
	stepFailed: replyFailed, stepStarting: replyStarting, stepOverdue: replyOverdue, stepRunning: replyRunning,
	stepStopping: replyStopping, stepMoving: replyMoving, stepCancelling: replyCancelling,
	stepCancelled: replyCancelled}

// rowAfter is the row session id is in, or "" once it is removed.
func rowAfter(t *testing.T, s *Service, id string) step {
	t.Helper()
	if _, err := s.readCatalog(id); os.IsNotExist(err) {
		return ""
	}
	row := stepOf(t, s, id)
	return row
}

// For a session in every row of the next-steps table, every command does
// exactly what outcomes says: the reply, whether it is a rejection, and the
// row the session is in afterwards.
func TestEachStateRefusesOrCarriesOutEachCommand(t *testing.T) {
	t.Parallel()
	if len(stateFixtures) != len(nextSteps) || len(outcomes) != len(nextSteps) {
		t.Fatalf("%d fixtures and %d outcome rows for %d table rows", len(stateFixtures), len(outcomes),
			len(nextSteps))
	}
	for row, fixture := range stateFixtures {
		for _, command := range commands {
			want, ok := outcomes[row][command]
			if !ok {
				t.Fatalf("no outcome for %s in %s", command, row)
			}
			t.Run(string(row)+"/"+command, func(t *testing.T) {
				t.Parallel()
				s, id := fixture(t)
				if before := rowAfter(t, s, id); before != row {
					t.Fatalf("the fixture reached %s, not %s", before, row)
				}
				q := Request{Action: command, ID: id}
				switch command {
				case ActionMove:
					q.OperationID, q.Destination = uuid.NewString(), testRemote
				case ActionPull, ActionResume, ActionStop:
					q.OperationID = uuid.NewString()
				case ActionCancel:
					// A cancellation already recorded is repeated, not
					// replaced: another is refused by the execution journal.
					q.OperationID = uuid.NewString()
					if x, err := s.coordinator.Inspect(id); err == nil && x.CancelOperation != "" {
						q.OperationID = x.CancelOperation
					}
				}
				r := exchange(t, s, q)
				reply := strings.ReplaceAll(want.reply, "ID", id)
				if r.Error != reply || r.Rejected != want.rejected {
					t.Fatalf("%s replied rejected=%v %q, want rejected=%v %q", command, r.Rejected, r.Error,
						want.rejected, reply)
				}
				if after := rowAfter(t, s, id); after != want.after {
					t.Fatalf("%s left the session in %q, want %q", command, after, want.after)
				}
			})
		}
	}
}

// A row's reply names only commands that its row does not refuse with that
// reply, except rm after the cancellation recover finishes, so the advice
// never names a command that is refused for the reason it gives.
func TestEachRowsAdviceNamesCommandsItAdmits(t *testing.T) {
	t.Parallel()
	for row, reply := range rowReplies {
		for _, command := range named(reply) {
			if row == stepCancelling && command == ActionRemove {
				continue
			}
			if o, ok := outcomes[row][command]; ok && o.rejected && o.reply == reply {
				t.Errorf("%s's reply names %s, which it refuses", row, command)
			}
		}
	}
}

// No row waits on the deadline to end (ADR 0060), and none outlasts its
// cause. The catalogue entry has no deadline column: the execution journal's
// admit refuses the events that start work, and here the deadline only turns
// a starting session overdue. Nothing is terminal; a stopped or cancelled
// session's exit is a request its owner makes. An exit is a command the
// next-steps table does not refuse that, once the fixture's cause is removed,
// the service carries out and that leaves the row; a command that fails and
// leaves the row as it was is not one. Every row's exit is driven with a
// deadline that has passed and with none, except that a starting session
// past its deadline is overdue, and nothing is overdue without one.
func TestEveryRowsExitLeavesItPastTheDeadlineAndWithoutOne(t *testing.T) {
	t.Parallel()
	if len(rowFixtures) != len(nextSteps) {
		t.Fatalf("%d fixtures for %d table rows", len(rowFixtures), len(nextSteps))
	}
	for name, st := range derivedStates(t) {
		if _, ok := rowFixtures[st.step()]; !ok {
			t.Errorf("%s derives row %s, which has no fixture", name, st.step())
		}
	}
	for row, f := range rowFixtures {
		if !slices.Contains(f.clocks, clockPassed) && row != stepStarting ||
			!slices.Contains(f.clocks, clockNone) && row != stepOverdue {
			t.Errorf("row %s is not driven past the deadline and without one: %v", row, f.clocks)
		}
		for _, c := range f.clocks {
			if c == clockAhead {
				continue
			}
			t.Run(string(row)+"/"+string(c), func(t *testing.T) {
				t.Parallel()
				s, id, exit := f.build(t, c)
				if before := rowAfter(t, s, id); before != row {
					t.Fatalf("the fixture reached %s, not %s", before, row)
				}
				checkClock(t, s, id, c)
				q := exit()
				if slices.Contains(nextSteps[row].refuses, q.Action) {
					t.Fatalf("row %s refuses its exit %s", row, q.Action)
				}
				r := exchange(t, s, q)
				if after := rowAfter(t, s, id); after == row {
					t.Fatalf("row %s has no exit with the deadline %s: %s left it there: %q", row, c, q.Action,
						r.Error)
				}
			})
		}
	}
}

// checkClock checks that the deadline of session id's current execution, or
// of its last stop when it has none, has passed under clockPassed and is
// unset under clockNone.
func checkClock(t *testing.T, s *Service, id string, c deadlineClock) {
	t.Helper()
	entry, err := s.readCatalog(id)
	if err != nil {
		t.Fatal(err)
	}
	until := entry.Committed.Launch.Deadline
	if entry.Current != "" {
		r, err := s.coordinator.Inspect(entry.Current)
		if err != nil {
			t.Fatal(err)
		}
		until = r.Deadline
	}
	passed := s.coordinator.PastDeadline(session.Record{Deadline: until})
	if c == clockPassed && !passed || c == clockNone && !until.IsZero() {
		t.Fatalf("deadline %v under clock %s", until, c)
	}
}
