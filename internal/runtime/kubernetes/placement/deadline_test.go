package placement

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/testutil"
)

// passDeadline sets up a visit with a real deadline and returns once that
// deadline has passed by the machine's clock, which the placement, the
// registry, waitPod's bound and deadline.Context all read
// (testutil.PassDeadline): set-up that the deadline overtook starts again
// with the deadline twice as far ahead.
func passDeadline(t *testing.T, ahead time.Duration, setUp func(ahead time.Duration) (time.Time, error)) {
	t.Helper()
	// setup's checkpoint is built once per test binary, which takes seconds;
	// build it before the first deadline.
	fixtureArtifactKind(t, harness.NativeCodex)
	testutil.PassDeadline(t, ahead, setUp)
}

// A preparation that never completed is aborted after its deadline. The
// registration's table admits cancel then, but the placement could still
// refuse before it reaches the registry; this drives the placement itself.
func TestAbortPreparationAfterDeadline(t *testing.T) {
	t.Parallel()
	var p *Placement
	var kube *admissionClient
	var l session.Launch
	passDeadline(t, time.Second, func(ahead time.Duration) (time.Time, error) {
		var data []byte
		p, kube, _, l, data = setup(t)
		l.Deadline = time.Now().Add(ahead).UTC()
		kube.queued = true
		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()
		if _, err := p.Prepare(ctx, l, data); err == nil {
			return l.Deadline, errors.New("queued preparation reported an incarnation")
		}
		local, err := p.load(l)
		if err != nil {
			return l.Deadline, err
		}
		if r, err := p.registry.Get(t.Context(), local.Reference); err != nil || r.Phase != registry.Bound {
			return l.Deadline, errors.Join(errors.New("queued registration is not bound"), err)
		}
		return l.Deadline, nil
	})
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
	var p *Placement
	var l session.Launch
	var i session.Incarnation
	var data []byte
	passDeadline(t, 500*time.Millisecond, func(ahead time.Duration) (time.Time, error) {
		p, _, _, l, data = setup(t)
		l.Deadline = time.Now().Add(ahead).UTC()
		var err error
		if i, err = p.Prepare(t.Context(), l, data); err != nil {
			return l.Deadline, err
		}
		return l.Deadline, p.Activate(t.Context(), l, i)
	})
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
	var p *Placement
	var l session.Launch
	var i session.Incarnation
	passDeadline(t, 500*time.Millisecond, func(ahead time.Duration) (time.Time, error) {
		var data []byte
		p, _, _, l, data = setup(t)
		l.Deadline = time.Now().Add(ahead).UTC()
		var err error
		i, err = p.Prepare(t.Context(), l, data)
		return l.Deadline, err
	})
	o, err := p.Abort(t.Context(), l, i)
	if err != nil || o.State != session.PlacementStopped || o.Stopped == nil || !o.Stopped.ActualStopped ||
		o.Stopped.Receipt == nil {
		t.Fatalf("abort after the deadline: %+v %v", o, err)
	}
}
