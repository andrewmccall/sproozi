package work

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// runningTask submits a task whose turn the placement reports as still
// running, and waits until the driver has waited for it a few times.
func runningTask(t *testing.T) (*Owner, *fixturePlacement, View) {
	t.Helper()
	c, p := fixture(t)
	p.turnPending, p.turnPace = 1<<30, 10*time.Millisecond
	o := openOwner(t, c)
	v := submit(t, o)
	ctx, cancel := untilDeadline(t)
	defer cancel()
	for {
		p.mu.Lock()
		stops := p.stops
		p.mu.Unlock()
		if stops >= 3 {
			return o, p, v
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the driver waited for the task's turn %d times", stops)
		case <-time.After(time.Millisecond):
		}
	}
}

// A task whose turn is running reports active with no problem: the driver's
// wait for the turn to complete is not a stop anyone asked for. Once the
// turn completes, the task stops and is retained.
func TestRunningTaskReportsActiveWithoutAProblem(t *testing.T) {
	t.Parallel()
	o, p, v := runningTask(t)
	for range 3 {
		got, err := o.Info(t.Context(), v.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Phase != phaseActive || got.Problem != "" {
			t.Fatalf("running task reported %s with problem %q", got.Phase, got.Problem)
		}
	}
	listed, err := o.List(t.Context(), SessionQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Phase != phaseActive || listed[0].Problem != "" {
		t.Fatalf("running task listed as %+v", listed)
	}
	p.mu.Lock()
	p.turnPending = 0
	p.mu.Unlock()
	if done := waitPhase(t, o, v.ID, phaseRetained); done.Problem != "" || done.Result != testResult {
		t.Fatalf("completed task: %+v", done)
	}
}

// A cancellation a caller asks for while the task's turn runs is reported
// as a cancellation, never as running.
func TestCancelWhileATaskRunsReportsTheCancellation(t *testing.T) {
	t.Parallel()
	o, _, v := runningTask(t)
	got, err := o.Cancel(t.Context(), v.ID, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != phaseCancelling && got.Phase != phaseCancelled {
		t.Fatalf("cancelled task reported %s", got.Phase)
	}
	waitPhase(t, o, v.ID, phaseCancelled)
}

// A stop an interactive session's owner asks for, while the agent's turn
// runs, is reported as stopping, with what it waits for as its problem.
func TestOwnerStopWaitingForTheTurnReportsStopping(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	o := openOwner(t, c)
	v := startInteractive(t, o, interactiveRequest())
	p.mu.Lock()
	p.turnPending = 1
	p.mu.Unlock()
	op := uuid.NewString()
	got, err := o.Stop(t.Context(), v.ID, op)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != phaseStopping || got.Problem != problemTurnPending {
		t.Fatalf("owner stop waiting for the turn reported %s with problem %q", got.Phase, got.Problem)
	}
	if got, err = o.Stop(t.Context(), v.ID, op); err != nil || got.Phase != phaseRetained {
		t.Fatalf("repeated stop: %s, %v", got.Phase, err)
	}
}
