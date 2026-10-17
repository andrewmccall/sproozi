package session

import (
	"testing"

	"github.com/google/uuid"
)

// A saved phase must describe the record's structure. Each case starts from a
// real active record and introduces one contradiction that save and load reject.
func TestRecordRejectsPhaseThatContradictsItsStructure(t *testing.T) {
	c, _, _, _, start := setup(t)
	active, err := c.Move(t.Context(), start.Source.SessionID, uuid.NewString(), testRemote)
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
