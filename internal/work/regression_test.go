package work

import (
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/google/uuid"
)

// waitDriver waits until no background driver runs for id and returns the
// session's view then.
func waitDriver(t *testing.T, o *Owner, id string) View {
	t.Helper()
	ctx, cancel := untilDeadline(t)
	defer cancel()
	for {
		o.mu.Lock()
		running := o.running[id] != nil
		o.mu.Unlock()
		if !running {
			v, err := o.Info(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
		select {
		case <-ctx.Done():
			t.Fatal("driver did not settle")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// A task whose turn outlasts the supervisor's quiesce wait keeps stopping
// until the turn completes; it is never reported as an unknown stop.
func TestTaskLongerThanTheQuiesceWaitKeepsStopping(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	p.turnPending = 2
	o, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	v := submit(t, o)
	ctx, cancel := untilDeadline(t)
	defer cancel()
	for {
		got, err := o.Info(ctx, v.ID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(string(got.Phase), "unknown") {
			t.Fatalf("a turn still running was reported as %s: %s", got.Phase, got.Problem)
		}
		if got.Phase == phaseRetained {
			if got.Result != testResult {
				t.Fatalf("retained without its result: %+v", got)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("task stayed %s: %s", got.Phase, got.Problem)
		case <-time.After(time.Millisecond):
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stops != 3 || p.active != 1 {
		t.Fatalf("stops=%d activations=%d, want 3 and 1", p.stops, p.active)
	}
}

// An unstarted task whose deadline passed is cancelled with that problem, and
// recovering it does not start it again.
func TestExpiredUnstartedTaskIsCancelled(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	c.MaxDuration = 0
	o, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	v, err := o.Submit(t.Context(), Submit{RequestID: uuid.NewString(), Repository: repositorySproozi,
		Harness: harness.NativeCodex, Executor: executorKubernetes, Task: fixtureTask, MaxDuration: "1ns"})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		got := waitDriver(t, o, v.ID)
		if got.Phase != phaseCancelled || !strings.Contains(got.Problem, "deadline") {
			t.Fatalf("expired task is %s: %q", got.Phase, got.Problem)
		}
		if _, err = o.Recover(t.Context(), v.ID); err != nil {
			t.Fatal(err)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.initial != 0 || p.active != 0 {
		t.Fatalf("expired task started: prepared=%d activated=%d", p.initial, p.active)
	}
}

// A cancellation that arrives once the task's stop has succeeded never
// discards the retained result: the session stays retained and the
// cancellation is refused.
func TestCancelNeverDiscardsARetainedResult(t *testing.T) {
	t.Parallel()
	for range 3 {
		c, p := fixture(t)
		returning := make(chan struct{})
		p.stopReturning = returning
		o, err := Open(c)
		if err != nil {
			t.Fatal(err)
		}
		v := submit(t, o)
		ctx, cancel := untilDeadline(t)
		select {
		case <-returning:
		case <-ctx.Done():
			t.Fatal("task never stopped")
		}
		cancel()
		_, cancelErr := o.Cancel(t.Context(), v.ID, uuid.NewString())
		got := waitDriver(t, o, v.ID)
		if got.Phase != phaseRetained || got.Result != testResult || got.CheckpointDigest == "" {
			t.Fatalf("cancel after a completed stop left %s: %+v", got.Phase, got)
		}
		if OutcomeCode(cancelErr) != profile.CodeConflict {
			t.Fatalf("cancel of retained work: %v, want conflict", cancelErr)
		}
		if err = o.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
