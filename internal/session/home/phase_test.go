package home

import (
	"regexp"
	"slices"
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
		"unclaimed":                        {c: &localSession{}},
		"stopped":                          {c: claimed(nil)},
		"stopped after a failure":          {c: claimed(failed)},
		"stopped at the deadline":          {c: claimed(late)},
		"idle current":                     {c: claimed(nil), current: record(session.PhaseIdle, visit(nil))},
		"idle current after a failure":     {c: claimed(nil), current: record(session.PhaseIdle, visit(failed))},
		"idle current, no visit, failed":   {c: claimed(failed), current: record(session.PhaseIdle)},
		"idle execution with no session":   {current: record(session.PhaseIdle)},
		"preparing":                        {c: claimed(nil), current: record(session.PhasePreparing)},
		"preparing past the deadline":      {c: claimed(nil), current: record(session.PhasePreparing, expired)},
		"activating":                       {c: claimed(nil), current: record(session.PhaseActivating)},
		"activating past the deadline":     {c: claimed(nil), current: record(session.PhaseActivating, expired)},
		"activating, cancel requested":     {c: claimed(nil), current: record(session.PhaseActivating, expired, cancelRequested)},
		"active writer":                    {c: claimed(nil), current: record(session.PhaseActive)},
		"stopping":                         {c: claimed(nil), current: record(session.PhaseStopping, finalizing)},
		"stopping to move":                 {c: claimed(nil), current: record(session.PhaseStopping, moving)},
		"unknown-stop":                     {c: claimed(nil), current: record(session.PhaseUnknownStop, finalizing)},
		"unknown-stop, cancel requested":   {c: claimed(nil), current: record(session.PhaseUnknownStop, cancelRequested)},
		"unknown-usage":                    {c: claimed(nil), current: record(session.PhaseUnknownUsage, moving)},
		"unknown-usage, cancel requested":  {c: claimed(nil), current: record(session.PhaseUnknownUsage, cancelRequested)},
		"cancelling":                       {c: claimed(nil), current: record(session.PhaseCancelling, cancelRequested)},
		"cancelled":                        {c: claimed(nil), current: record(session.PhaseCancelled, cancelRequested)},
		"unclaimed with a current, unseen": {c: &localSession{}, current: record(session.PhaseActive)},
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
		"unclaimed":                        PhaseImporting,
		"stopped":                          PhaseRetained,
		"stopped after a failure":          PhaseFailed,
		"stopped at the deadline":          PhaseRetained,
		"idle current":                     PhaseRetained,
		"idle current after a failure":     PhaseFailed,
		"idle current, no visit, failed":   PhaseFailed,
		"idle execution with no session":   PhaseRetained,
		"preparing":                        Phase(session.PhasePreparing),
		"preparing past the deadline":      Phase(session.PhasePreparing),
		"activating":                       Phase(session.PhaseActivating),
		"activating past the deadline":     Phase(session.PhaseActivating),
		"activating, cancel requested":     Phase(session.PhaseActivating),
		"active writer":                    Phase(session.PhaseActive),
		"stopping":                         Phase(session.PhaseStopping),
		"stopping to move":                 Phase(session.PhaseStopping),
		"unknown-stop":                     Phase(session.PhaseUnknownStop),
		"unknown-stop, cancel requested":   Phase(session.PhaseUnknownStop),
		"unknown-usage":                    Phase(session.PhaseUnknownUsage),
		"unknown-usage, cancel requested":  Phase(session.PhaseUnknownUsage),
		"cancelling":                       Phase(session.PhaseCancelling),
		"cancelled":                        Phase(session.PhaseCancelled),
		"unclaimed with a current, unseen": PhaseImporting,
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

// Every state has one row of the next-steps table. Its refusals and the
// commands its advice names are as listed, and the advice names no command
// the state refuses except one it says to run once the first has finished.
func TestNextStepsNameWhatEachStateAdmits(t *testing.T) {
	t.Parallel()
	want := map[step]struct {
		refuses, names, afterwards []string
	}{
		stepImporting: {refuses: []string{ActionResume, ActionAttach, ActionStop, ActionMove, ActionRecover, ActionCancel,
			ActionRemove}, names: []string{ActionPull}},
		stepRetained: {refuses: []string{ActionAttach, ActionMove, ActionRecover, ActionCancel},
			names: []string{ActionResume}},
		stepFailed: {refuses: []string{ActionAttach, ActionMove, ActionRecover, ActionCancel},
			names: []string{ActionStatus, ActionResume, ActionRemove}},
		stepStarting: {refuses: []string{ActionResume, ActionAttach, ActionRemove},
			names: []string{ActionRecover, ActionCancel}},
		stepOverdue: {refuses: []string{ActionAttach, ActionRemove}, names: []string{ActionResume, ActionCancel}},
		stepRunning: {refuses: []string{ActionResume, ActionRemove},
			names: []string{ActionAttach, ActionStop, ActionCancel}},
		stepStopping: {refuses: []string{ActionResume, ActionAttach, ActionRemove},
			names: []string{ActionStop, ActionCancel}},
		stepMoving: {refuses: []string{ActionResume, ActionAttach, ActionRemove},
			names: []string{ActionRecover, ActionCancel}},
		stepCancelling: {refuses: []string{ActionResume, ActionAttach, ActionRemove},
			names: []string{ActionRecover, ActionRemove}, afterwards: []string{ActionRemove}},
		// "run" starts a new session; it is no command on this one.
		stepCancelled: {refuses: []string{ActionResume, ActionAttach}, names: []string{ActionRemove, "run"}},
	}
	if len(nextSteps) != len(want) {
		t.Fatalf("the table has %d rows, the test %d", len(nextSteps), len(want))
	}
	steps := map[step]string{}
	for name, st := range derivedStates(t) {
		steps[st.step()] = name
		row, ok := nextSteps[st.step()]
		if !ok {
			t.Errorf("%s: no row for %q", name, st.step())
			continue
		}
		if st.phase == PhaseFailed && st.exit == nil {
			t.Errorf("%s: failed without an exit", name)
		}
		w := want[st.step()]
		text := row.problem("ID", st)
		if got := named(text); !slices.Equal(got, w.names) {
			t.Errorf("%s (%s): advice %q names %v, want %v", name, st.step(), text, got, w.names)
		}
		for _, command := range commands {
			refused := refusal("ID", st, command) != nil
			if refused != slices.Contains(w.refuses, command) {
				t.Errorf("%s (%s): %s refused %v", name, st.step(), command, refused)
			}
			if refused && slices.Contains(w.names, command) && !slices.Contains(w.afterwards, command) {
				t.Errorf("%s (%s): advice names %s, which it refuses", name, st.step(), command)
			}
		}
	}
	for s := range want {
		if _, ok := steps[s]; !ok {
			t.Errorf("no derived state reaches %q", s)
		}
	}
}

// The service refuses each command a session's state refuses with exactly
// that state's advice, for sessions brought to those states by commands.
func TestServiceRefusalsAreTheTablesAdvice(t *testing.T) {
	t.Parallel()
	s, local, source := importedFixture(t)
	repo, _, _ := testWorkspace(t)
	sessions := map[string]string{}
	// importing: a claim the source refuses.
	source.denyClaim = true
	if _, err := s.Pull(t.Context(), source.export.ID, source.export.OperationID, testRemote); err == nil {
		t.Fatal("a refused claim finished the pull")
	}
	sessions["importing"] = source.export.ID
	running := startedOn(t, s, repo, testLocal)
	sessions["running"] = running
	retained := startedOn(t, s, repo, testLocal)
	if _, err := s.Stop(t.Context(), retained, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	sessions["retained"] = retained
	cancelled := startedOn(t, s, repo, testLocal)
	if _, err := s.Cancel(t.Context(), cancelled, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	sessions["cancelled"] = cancelled
	starting := uuid.NewString()
	local.lostActivate = true
	if _, err := s.Start(t.Context(), starting, uuid.NewString(), repo, "", nil, nil); err == nil {
		t.Fatal("a lost activation reply started the session")
	}
	sessions["starting"] = starting
	for name, id := range sessions {
		c, err := s.readCatalog(id)
		if err != nil {
			t.Fatal(err)
		}
		st, _, err := s.sessionState(c)
		if err != nil || string(st.step()) != name {
			t.Fatalf("session %s is %+v, not %s: %v", id, st, name, err)
		}
		for _, command := range commands {
			want := refusal(id, st, command)
			if want == nil {
				continue
			}
			q := Request{Action: command, ID: id}
			switch command {
			case ActionMove:
				q.OperationID, q.Destination = uuid.NewString(), testRemote
			case ActionPull, ActionResume, ActionStop, ActionCancel:
				q.OperationID = uuid.NewString()
			}
			r := exchange(t, s, q)
			if !r.Rejected || r.Error != want.Error() {
				t.Errorf("%s (%s): %s replied %+v, want %q", name, st.phase, command, r, want)
			}
		}
	}
}
