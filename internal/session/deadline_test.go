package session

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// An execution started without a deadline runs, moves and stops however long
// it has run, and every visit keeps having none (ADR 0056).
func TestExecutionWithoutDeadlineMovesAndStops(t *testing.T) {
	t.Parallel()
	c, dir, local, _, start := setup(t)
	source := start.Source
	source.ExecutionID, source.OperationID, source.AllocationID = uuid.NewString(), uuid.NewString(), uuid.NewString()
	source.Deadline = time.Time{}
	seeded := seedExecution(t, c, local, source, start.Input)
	c.now = func() time.Time { return time.Now().Add(100 * 365 * 24 * time.Hour) }
	r, err := c.Move(context.Background(), seeded.Source.ExecutionID, uuid.NewString(), testRemote)
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != PhaseActive || !r.Deadline.IsZero() || !r.Active.Launch.Deadline.IsZero() {
		t.Fatalf("move without deadline: phase %s deadline %v %v", r.Phase, r.Deadline, r.Active.Launch.Deadline)
	}
	if r, err = c.Finalize(context.Background(), r.ID, uuid.NewString()); err != nil || r.Phase != PhaseIdle {
		t.Fatalf("stop without deadline: %+v %v", r.Phase, err)
	}
	// The journal without a deadline reopens.
	_ = c.Close()
	reopened, err := Open(dir, map[string]Placement{testLocal: local}, testMaxCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if got, err := reopened.Inspect(r.ID); err != nil || !got.Deadline.IsZero() {
		t.Fatalf("reopened journal: %v %v", got.Deadline, err)
	}
}

// An interrupted start without a deadline is never ended for lateness:
// recovery, however late, prepares and activates it (ADR 0060).
func TestInterruptedStartWithoutDeadlineIsNeverAbandoned(t *testing.T) {
	t.Parallel()
	c, _, local, _, start := setup(t)
	first := start.Source
	first.ExecutionID, first.OperationID, first.AllocationID = uuid.NewString(), uuid.NewString(), uuid.NewString()
	first.Deadline = time.Time{}
	if _, err := c.Create(first, start.Input, ""); err != nil {
		t.Fatal(err)
	}
	local.lostPrepare = true
	if _, err := c.Recover(t.Context(), first.ExecutionID); err == nil {
		t.Fatal("fixture did not lose the prepare reply")
	}
	c.now = func() time.Time { return time.Now().Add(100 * 365 * 24 * time.Hour) }
	if c.PastDeadline(Record{}) {
		t.Fatal("no deadline passed")
	}
	r, err := c.Recover(t.Context(), first.ExecutionID)
	if err != nil || r.Phase != PhaseActive || local.aborted != 0 {
		t.Fatalf("late recovery without a deadline: %s %v", r.Phase, err)
	}
}
