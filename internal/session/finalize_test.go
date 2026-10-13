package session

import (
	"testing"

	"github.com/google/uuid"
)

func TestFinalizeRecoversStopWithoutPreparingDestination(t *testing.T) {
	c, _, local, remote, start := setup(t)
	id := start.Source.SessionID
	if _, err := c.Move(t.Context(), id, uuid.NewString(), testRemote); err != nil {
		t.Fatal(err)
	}
	prepared := local.prepared + remote.prepared
	op := uuid.NewString()
	remote.lostStop = true
	r, err := c.Finalize(t.Context(), id, op)
	if err == nil || r.Phase != PhaseUnknownStop || r.PendingOperation != op {
		t.Fatalf("lost stop did not retain finalization: %+v %v", r, err)
	}
	r, err = c.Recover(t.Context(), id)
	if err != nil || r.Phase != PhaseIdle || r.Active != nil || r.PendingOperation != "" || !r.Operations[len(r.Operations)-1].Finalize {
		t.Fatalf("recover did not finish stopped execution: %+v %v", r, err)
	}
	if local.prepared+remote.prepared != prepared || len(r.Visits) != 2 || r.Remaining[testCapability].Units.Value != 13 {
		t.Fatalf("finalization prepared writer or changed charges: %+v", r)
	}
	replay, err := c.Finalize(t.Context(), id, op)
	if err != nil || replay.Remaining[testCapability].Units.Value != 13 || remote.stopped != 1 {
		t.Fatalf("finalization double-charged: %+v %v", replay, err)
	}
	if _, err = c.Move(t.Context(), id, op, testLocal); err == nil {
		t.Fatal("stop operation reinterpreted as a move")
	}
}

func TestFinalizeMissingAccountingRetainsAllocation(t *testing.T) {
	c, _, _, remote, start := setup(t)
	if _, err := c.Move(t.Context(), start.Source.SessionID, uuid.NewString(), testRemote); err != nil {
		t.Fatal(err)
	}
	remote.missingUsage = true
	r, err := c.Finalize(t.Context(), start.Source.SessionID, uuid.NewString())
	if err == nil || r.Phase != PhaseUnknownUsage || r.Active == nil || r.Remaining[testCapability].Units.Value != 18 {
		t.Fatalf("unknown accounting refunded capacity: %+v %v", r, err)
	}
}
