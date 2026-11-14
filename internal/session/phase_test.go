package session

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/testutil"
	"github.com/google/uuid"
)

// placementOutcome is one of the three outcomes Placement.Stop and
// Placement.Abort report: the writer fenced with its usage sealed, the writer
// fenced with its usage unsealed, or the writer's stop unknown.
type placementOutcome string

const (
	outcomeSealed   placementOutcome = "sealed"
	outcomeUnsealed placementOutcome = "unsealed"
	outcomeUnknown  placementOutcome = "unknown"
)

// configure makes p's next Stop and Abort report outcome.
func (o placementOutcome) configure(p *fakePlacement) {
	p.unsealed, p.unknownStop = o == outcomeUnsealed, o == outcomeUnknown
	p.abortSealed, p.abortUnreachable = o == outcomeSealed, o == outcomeUnknown
}

// Every operation that stops a writer ends in exactly one phase for each
// placement outcome, and recovering it ends in exactly one phase for each
// outcome of the retried call. A writer once proven stopped stays proven
// stopped, whatever a retry reports.
func TestStopOutcomesProduceOnePhaseEach(t *testing.T) {
	t.Parallel()
	// want[op][outcome] is the phase after op, and after recovery whose
	// retried call reports outcome, starting from an unresolved stop.
	const opMove, opFinalize, opCancel = "move", "finalize", "cancel"
	want := map[string]map[placementOutcome]Phase{
		opMove:     {outcomeSealed: PhaseActive, outcomeUnsealed: PhaseUnknownUsage, outcomeUnknown: PhaseUnknownStop},
		opFinalize: {outcomeSealed: PhaseIdle, outcomeUnsealed: PhaseUnknownUsage, outcomeUnknown: PhaseUnknownStop},
		opCancel:   {outcomeSealed: PhaseCancelled, outcomeUnsealed: PhaseUnknownUsage, outcomeUnknown: PhaseUnknownStop},
	}
	outcomes := []placementOutcome{outcomeSealed, outcomeUnsealed, outcomeUnknown}
	for op, phases := range want {
		for _, first := range outcomes {
			for _, retry := range outcomes {
				t.Run(op+"/"+string(first)+"/then-"+string(retry), func(t *testing.T) {
					t.Parallel()
					c, _, _, remote, start := setup(t)
					id := start.Source.ExecutionID
					if r, err := c.Move(t.Context(), id, uuid.NewString(), testRemote); err != nil || r.Phase != PhaseActive {
						t.Fatalf("move to remote: %s %v", r.Phase, err)
					}
					first.configure(remote)
					var r Record
					switch op {
					case opMove:
						r, _ = c.Move(t.Context(), id, uuid.NewString(), testLocal)
					case opFinalize:
						r, _ = c.Finalize(t.Context(), id, uuid.NewString())
					case opCancel:
						r, _ = c.Cancel(t.Context(), id, uuid.NewString(), nil)
					}
					if r.Phase != phases[first] {
						t.Fatalf("%s with %s outcome: phase %s, want %s", op, first, r.Phase, phases[first])
					}
					retry.configure(remote)
					r, _ = c.Recover(t.Context(), id)
					expected := phases[retry]
					switch {
					case first == outcomeSealed:
						// Nothing is left to recover.
						expected = phases[first]
					case first == outcomeUnsealed && retry == outcomeUnknown:
						// The retry cannot unprove the earlier fence.
						expected = PhaseUnknownUsage
					}
					if r.Phase != expected {
						t.Fatalf("recover with %s outcome: phase %s, want %s", retry, r.Phase, expected)
					}
					if stored, err := c.Inspect(id); err != nil || stored.Phase != r.Phase {
						t.Fatalf("stored phase %s %v, returned %s", stored.Phase, err, r.Phase)
					}
				})
			}
		}
	}
}

// A stop the placement reports still waiting for the native turn proves the
// writer runs with input gated, so it is stopping whatever the previous
// attempt said, and a later stop that seals finishes the same operation.
func TestStopStillWaitingForTurnIsStopping(t *testing.T) {
	t.Parallel()
	for _, first := range []struct {
		name string
		set  func(*fakePlacement)
		want Phase
	}{
		{"waiting", func(p *fakePlacement) { p.pendingTurn = true }, PhaseStopping},
		{"unknown then waiting", func(p *fakePlacement) { p.unknownStop = true }, PhaseUnknownStop},
	} {
		t.Run(first.name, func(t *testing.T) {
			t.Parallel()
			c, _, _, remote, start := setup(t)
			id := start.Source.ExecutionID
			if r, err := c.Move(t.Context(), id, uuid.NewString(), testRemote); err != nil || r.Phase != PhaseActive {
				t.Fatalf("move to remote: %s %v", r.Phase, err)
			}
			first.set(remote)
			op := uuid.NewString()
			if r, _ := c.Finalize(t.Context(), id, op); r.Phase != first.want {
				t.Fatalf("first stop: phase %s, want %s", r.Phase, first.want)
			}
			remote.unknownStop, remote.pendingTurn = false, true
			r, err := c.Recover(t.Context(), id)
			if !errors.Is(err, harness.ErrNativeTurnPending) || r.Phase != PhaseStopping {
				t.Fatalf("stop still waiting: phase %s %v, want %s", r.Phase, err, PhaseStopping)
			}
			if stored, err := c.Inspect(id); err != nil || stored.Phase != PhaseStopping {
				t.Fatalf("stored phase %s %v", stored.Phase, err)
			}
			remote.pendingTurn = false
			if r, err = c.Finalize(t.Context(), id, uuid.NewString()); err != nil || r.Phase != PhaseIdle ||
				r.Operations[len(r.Operations)-1].ID != op {
				t.Fatalf("finished stop: phase %s %v", r.Phase, err)
			}
		})
	}
}

// Preparation and activation each end in exactly one phase on success and on
// failure, and recovery finishes them.
func TestPrepareAndActivateOutcomesProduceOnePhaseEach(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		configure  func(*fakePlacement)
		want       Phase
		recovered  Phase
		pastExpiry bool
	}{
		{name: "prepare and activate succeed", configure: func(*fakePlacement) {}, want: PhaseActive, recovered: PhaseActive},
		{name: "prepare reply lost", configure: func(p *fakePlacement) { p.lostPrepare = true }, want: PhasePreparing, recovered: PhaseActive},
		{name: "prepare fails partway", configure: func(p *fakePlacement) { p.partialPrepare = true }, want: PhasePreparing, recovered: PhaseActive},
		{name: "activate reply lost", configure: func(p *fakePlacement) { p.lostActivate = true }, want: PhaseActivating, recovered: PhaseActive},
		{name: "deadline passed", configure: func(*fakePlacement) {}, want: PhasePreparing, recovered: PhasePreparing, pastExpiry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, _, _, remote, start := setup(t)
			if tc.pastExpiry {
				c.now = func() time.Time { return start.Source.Deadline }
			}
			tc.configure(remote)
			r, err := c.Move(t.Context(), start.Source.ExecutionID, uuid.NewString(), testRemote)
			if r.Phase != tc.want || (err == nil) != (tc.want == PhaseActive) {
				t.Fatalf("move: phase %s %v, want %s", r.Phase, err, tc.want)
			}
			if r, _ = c.Recover(t.Context(), r.ID); r.Phase != tc.recovered {
				t.Fatalf("recover: phase %s, want %s", r.Phase, tc.recovered)
			}
		})
	}
}

// Create retains a preparing first visit; cancelling an idle execution
// finishes at once because nothing runs.
func TestCreateAndIdleCancelPhases(t *testing.T) {
	t.Parallel()
	c, _, local, _, start := setup(t)
	source := start.Source
	source.ExecutionID, source.OperationID, source.AllocationID = uuid.NewString(), uuid.NewString(), uuid.NewString()
	r, err := c.Create(source, start.Input, "")
	if err != nil || r.Phase != PhasePreparing {
		t.Fatalf("create: %s %v", r.Phase, err)
	}
	r, err = c.Cancel(t.Context(), start.Source.ExecutionID, uuid.NewString(), nil)
	if err != nil || r.Phase != PhaseCancelled || local.aborted != 0 {
		t.Fatalf("cancel idle: %s %v aborts %d", r.Phase, err, local.aborted)
	}
}

// Every declared transition names known phases, every phase is reachable
// from creation and every phase but cancelled has a way on.
func TestTransitionTableIsClosed(t *testing.T) {
	reached := map[Phase]bool{phaseNone: true}
	for changed := true; changed; {
		changed = false
		for ev, rule := range transitions {
			for from, to := range rule.to {
				if from != phaseNone && !from.valid() || !to.valid() {
					t.Fatalf("%s: unknown phase in %q -> %q", ev, from, to)
				}
				if reached[from] && !reached[to] {
					reached[to], changed = true, true
				}
			}
		}
	}
	for _, p := range phases {
		if !reached[p] {
			t.Errorf("phase %s is unreachable", p)
		}
		leaves := false
		for _, rule := range transitions {
			if to, ok := rule.to[p]; ok && to != p {
				leaves = true
			}
		}
		if leaves == (p == PhaseCancelled) {
			t.Errorf("phase %s: has a way on = %v", p, leaves)
		}
	}
}

// apply refuses a transition the table does not declare and leaves the
// record unchanged.
func TestApplyRefusesUndeclaredTransition(t *testing.T) {
	for _, tc := range []struct {
		from Phase
		ev   event
	}{
		{PhaseIdle, evActivated},
		{PhaseActive, evCreate},
		{PhaseCancelled, evMove},
		{PhaseCancelled, evCancel},
		{PhasePreparing, evStopped},
		{PhaseStopping, evPrepared},
		{PhaseUnknownStop, evFinalized},
		{PhaseCancelling, evMove},
	} {
		r := Record{Phase: tc.from}
		err := apply(&r, tc.ev)
		if err == nil || r.Phase != tc.from || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s on %s: %v, phase now %s", tc.ev, tc.from, err, r.Phase)
		}
	}
	r := Record{Phase: PhaseActive}
	if err := apply(&r, evMove); err != nil || r.Phase != PhaseStopping {
		t.Fatalf("declared move: %v %s", err, r.Phase)
	}
}

// Events that start work are refused once the deadline passes; events that
// stop, account or cancel are not. No deadline refuses nothing.
func TestDeadlineRefusesOnlyEventsThatStartWork(t *testing.T) {
	deadline := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	c := &Coordinator{now: func() time.Time { return deadline }}
	starting := []event{evCreate, evPrepared, evActivated}
	for ev, rule := range transitions {
		if rule.refusedAfterDeadline != slices.Contains(starting, ev) {
			t.Errorf("%s: refusedAfterDeadline = %v", ev, rule.refusedAfterDeadline)
		}
		for from := range rule.to {
			r := Record{Phase: from, Deadline: deadline}
			if err := c.admit(r, ev); (err != nil) != rule.refusedAfterDeadline {
				t.Errorf("%s from %s past the deadline: %v", ev, from, err)
			}
			r.Deadline = time.Time{}
			if err := c.admit(r, ev); err != nil {
				t.Errorf("%s from %s without a deadline: %v", ev, from, err)
			}
		}
	}
	if err := c.admit(Record{Phase: PhaseIdle}, evActivated); err == nil {
		t.Fatal("admitted an undeclared event")
	}
}

// A journal written before phases were derived may say cancelling for a
// record that already describes another phase. It loads as that phase.
func TestLegacyCancellingJournalLoadsAsItsDerivedPhase(t *testing.T) {
	t.Parallel()
	c, dir, _, _, start := setup(t)
	id := start.Source.ExecutionID
	r, err := c.Cancel(t.Context(), id, uuid.NewString(), nil)
	if err != nil || r.Phase != PhaseCancelled {
		t.Fatalf("cancel: %s %v", r.Phase, err)
	}
	r.Phase = PhaseCancelling
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = testutil.WriteFile(filepath.Join(dir, id+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Inspect(id); err != nil || got.Phase != PhaseCancelled {
		t.Fatalf("legacy journal: %s %v", got.Phase, err)
	}
}

func TestPhasePredicates(t *testing.T) {
	type flags struct{ live, stopping, starting, settled, unresolved bool }
	want := map[Phase]flags{
		PhaseIdle:         {settled: true},
		PhasePreparing:    {live: true, starting: true},
		PhaseActivating:   {live: true, starting: true},
		PhaseActive:       {live: true},
		PhaseStopping:     {live: true, stopping: true},
		PhaseUnknownStop:  {stopping: true, unresolved: true},
		PhaseUnknownUsage: {stopping: true, unresolved: true},
		PhaseCancelling:   {unresolved: true},
		PhaseCancelled:    {settled: true},
		"starting":        {},
		"":                {},
	}
	if len(want) != len(phases)+2 {
		t.Fatalf("predicate table covers %d phases, vocabulary has %d", len(want)-2, len(phases))
	}
	for p, w := range want {
		got := flags{p.Live(), p.Stopping(), p.Starting(), p.Settled(), p.Unresolved()}
		if got != w {
			t.Errorf("%q: got %+v, want %+v", p, got, w)
		}
	}
}
