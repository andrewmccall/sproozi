package placement

import (
	"context"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/session"
)

// waitPast waits until the wall clock, which the placement reads, is past
// deadline. The placement has no clock to replace: it reads time.Now, and
// the registry's clock is not reachable from here.
func waitPast(deadline time.Time) { time.Sleep(time.Until(deadline) + 50*time.Millisecond) }

// A preparation that never completed is aborted after its deadline. The
// registration's table admits cancel then, but the placement could still
// refuse before it reaches the registry; this drives the placement itself.
func TestAbortPreparationAfterDeadline(t *testing.T) {
	t.Parallel()
	p, kube, _, l, data := setup(t)
	l.Deadline = time.Now().Add(2 * time.Second).UTC()
	kube.queued = true
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if _, err := p.Prepare(ctx, l, data); err == nil {
		t.Fatal("queued preparation reported an incarnation")
	}
	if r := registration(t, p, l); r.Phase != registry.Bound {
		t.Fatalf("queued registration: %+v", r)
	}
	waitPast(l.Deadline)
	kube.reconcile = cancelReconcile(t, kube)
	if err := p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatalf("abort-preparation after the deadline: %v", err)
	}
	if r := registration(t, p, l); r.Phase != registry.Cancelled {
		t.Fatalf("registration after abort: %+v", r)
	}
}

// An active visit is stopped after its deadline, with its checkpoint and
// sealed usage, through the placement and not only the registration's table.
func TestStopAfterDeadline(t *testing.T) {
	t.Parallel()
	p, _, _, l, data := setup(t)
	l.Deadline = time.Now().Add(3 * time.Second).UTC()
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	waitPast(l.Deadline)
	s, err := p.Stop(t.Context(), l, i)
	if err != nil || !s.ActualStopped || s.Receipt == nil || string(s.Checkpoint) != string(data) {
		t.Fatalf("stop after the deadline: %+v %v", s, err)
	}
}

// A prepared visit is aborted after its deadline: that is how the execution
// journal ends a move or first visit that never activated, and how one is
// cancelled. The writer is proven stopped and its usage sealed.
func TestAbortAfterDeadline(t *testing.T) {
	t.Parallel()
	p, _, _, l, data := setup(t)
	l.Deadline = time.Now().Add(2 * time.Second).UTC()
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	waitPast(l.Deadline)
	o, err := p.Abort(t.Context(), l, i)
	if err != nil || o.State != session.PlacementStopped || o.Stopped == nil || !o.Stopped.ActualStopped ||
		o.Stopped.Receipt == nil {
		t.Fatalf("abort after the deadline: %+v %v", o, err)
	}
}
