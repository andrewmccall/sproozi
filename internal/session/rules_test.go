package session

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// A saved phase must describe the record's structure. Each case starts from a
// real active record and introduces one contradiction that save and load reject.
func TestRecordRejectsPhaseThatContradictsItsStructure(t *testing.T) {
	t.Parallel()
	c, _, _, _, start := setup(t)
	active, err := c.Move(t.Context(), start.Source.ExecutionID, uuid.NewString(), testRemote)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateRecord(active); err != nil {
		t.Fatalf("baseline active record rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Record)
	}{
		{"active phase without a writer", func(r *Record) { r.Active = nil; r.Epoch = r.Visits[len(r.Visits)-1].Launch.Epoch }},
		{"idle phase with a running writer", func(r *Record) { r.Phase = PhaseIdle }},
		{"unknown usage without a fenced writer", func(r *Record) { r.Phase = PhaseUnknownUsage }},
		{"cancelled phase without cancellation intent", func(r *Record) { r.Phase = PhaseCancelled }},
		{"cancellation intent on an active phase", func(r *Record) { r.CancelOperation = uuid.NewString() }},
		{"stopping phase with nothing pending", func(r *Record) { r.Phase = PhaseStopping }},
		{"unfinished operation that is not pending", func(r *Record) { r.Operations[len(r.Operations)-1].Stage = stageStop }},
		{"pending operation that already finished", func(r *Record) {
			r.PendingOperation = r.Operations[len(r.Operations)-1].ID
			r.Phase = PhaseStopping
		}},
		{"unknown phase", func(r *Record) { r.Phase = "paused" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := active
			r.Operations = append([]Operation(nil), active.Operations...)
			tc.mutate(&r)
			if err := validateRecord(r); err == nil {
				t.Fatalf("record accepted: phase=%s active=%v pending=%q cancel=%q", r.Phase, r.Active != nil, r.PendingOperation, r.CancelOperation)
			}
		})
	}
}

// A refused move or stop names the operation that is actually pending.
func TestPendingRecoveryNamesThePendingOperation(t *testing.T) {
	ops := []Operation{{ID: uuid.NewString()}, {ID: uuid.NewString()}, {ID: uuid.NewString(), Finalize: true}}
	for _, tc := range []struct {
		pending int
		thread  string
		want    string
	}{
		{0, "", "session: previous start requires recovery"},
		{0, uuid.NewString(), "session: previous resume requires recovery"},
		{1, uuid.NewString(), "session: previous move requires recovery"},
	} {
		err := pendingRecovery(Record{Operations: ops, PendingOperation: ops[tc.pending].ID, ThreadID: tc.thread})
		if !errors.Is(err, ErrRecoveryRequired) || err.Error() != tc.want {
			t.Fatalf("pending %d: got %v, want %q", tc.pending, err, tc.want)
		}
	}
	// A pending stop is continued by repeating the stop, not by recovery.
	if err := pendingRecovery(Record{Operations: ops, PendingOperation: ops[2].ID}); !errors.Is(err, ErrStopPending) {
		t.Fatalf("pending stop: got %v", err)
	}
}

// Only an unfinished cancellation points at recovery; a finished one is final.
func TestCancelledRefusalDependsOnFinishedCancellation(t *testing.T) {
	if err := cancelled(Record{Phase: PhaseCancelled}); !errors.Is(err, ErrSessionCancelled) {
		t.Fatalf("finished cancellation: got %v", err)
	}
	for _, phase := range []Phase{PhaseCancelling, PhaseUnknownStop, PhaseUnknownUsage} {
		if err := cancelled(Record{Phase: phase}); !errors.Is(err, ErrCancellationUnfinished) {
			t.Fatalf("%s: got %v", phase, err)
		}
	}
}
