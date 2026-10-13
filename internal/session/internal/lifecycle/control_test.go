package lifecycle

import (
	"context"
	"errors"
	"testing"
)

func TestCancellationTicketBlocksGracefulRetryUntilAllCallersFinish(t *testing.T) {
	var control Control
	run, finish, err := control.Begin(t.Context(), "session-a")
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	unrelated, err := control.RequestCancel("session-b", "cancel-b")
	if err != nil {
		t.Fatal(err)
	}
	if run.Err() != nil {
		t.Fatal("another session's cancellation interrupted the running operation")
	}
	unrelated()
	first, err := control.RequestCancel("session-a", "cancel-a")
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(run.Err(), context.Canceled) {
		t.Fatal("cancel ticket did not interrupt the graceful context")
	}
	second, err := control.RequestCancel("session-a", "cancel-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = control.RequestCancel("session-a", "different-cancel"); err == nil {
		t.Fatal("conflicting cancellation ID replaced a pending ticket")
	}
	finish()
	if _, _, err = control.Begin(t.Context(), "session-a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("graceful retry passed pending cancellation: %v", err)
	}
	first()
	if _, _, err = control.Begin(t.Context(), "session-a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("first duplicate caller removed another pending ticket: %v", err)
	}
	second()
	next, finishNext, err := control.Begin(t.Context(), "session-a")
	if err != nil {
		t.Fatal(err)
	}
	defer finishNext()
	if next.Err() != nil {
		t.Fatal("completed operation's cancellation handle leaked into a later operation")
	}
}
