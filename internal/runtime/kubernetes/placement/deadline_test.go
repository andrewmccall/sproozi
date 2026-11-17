package placement

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/session"
)

// passDeadline gives p a clock that reads the machine's clock until the
// returned function moves it a second past l's deadline, which setup puts
// half an hour ahead. The test, not the machine's speed, decides when the
// deadline passes for the placement. The registry keeps its own clock,
// which does not pass the deadline here; its table's rules after the
// deadline are tested in its own package.
func passDeadline(p *Placement, l session.Launch) (past func()) {
	var passed atomic.Bool
	p.now = func() time.Time {
		if passed.Load() {
			return l.Deadline.Add(time.Second)
		}
		return time.Now()
	}
	return func() { passed.Store(true) }
}

// The deadline tests replace the placement's clock; an opened placement's
// own clock is the machine's, read at the moment it judges a deadline.
func TestPlacementJudgesDeadlinesOnTheMachineClock(t *testing.T) {
	t.Parallel()
	p, _, _, _, _ := setup(t)
	before := time.Now()
	now := p.now()
	after := time.Now()
	if now.Before(before) || now.After(after) {
		t.Fatalf("placement clock read %s, want between %s and %s", now, before, after)
	}
}

// A preparation that never completed is aborted after its deadline. The
// registration's table admits cancel then, but the placement could still
// refuse before it reaches the registry; this drives the placement itself.
func TestAbortPreparationAfterDeadline(t *testing.T) {
	t.Parallel()
	p, kube, l, _ := queuedPreparation(t)
	passDeadline(p, l)()
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
	pastDeadline := passDeadline(p, l)
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	pastDeadline()
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
	pastDeadline := passDeadline(p, l)
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	pastDeadline()
	o, err := p.Abort(t.Context(), l, i)
	if err != nil || o.State != session.PlacementStopped || o.Stopped == nil || !o.Stopped.ActualStopped ||
		o.Stopped.Receipt == nil {
		t.Fatalf("abort after the deadline: %+v %v", o, err)
	}
}
