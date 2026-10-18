package docker

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session"
)

// activeFixture prepares and activates one visit whose stop can capture.
func activeFixture(t *testing.T, dir string) (*Placement, *fakeDocker, *fakeAuthority, session.Launch, session.Incarnation) {
	t.Helper()
	data := fixtureArtifact(t)
	l := fixtureLaunch(t, data)
	f := newFakeDocker()
	f.capture = data
	a := &fakeAuthority{engine: f}
	p := openFixture(t, dir, f, a)
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	return p, f, a, l, i
}

func assertReleased(t *testing.T, p *Placement, f *fakeDocker, l session.Launch) {
	t.Helper()
	if len(f.containers) != 0 || len(f.volumes) != 0 {
		t.Fatalf("visit left resources: %d containers, %d volumes", len(f.containers), len(f.volumes))
	}
	r, err := p.load(l)
	if err != nil || !r.Removing || !r.Removed {
		t.Fatalf("removal was not recorded: %+v %v", r, err)
	}
}

// assertAnsweredFromRecord replays Stop, Abort and Inspect after removal and
// requires the retained result without any engine call.
func assertAnsweredFromRecord(t *testing.T, p *Placement, f *fakeDocker, l session.Launch, i session.Incarnation, want session.Stopped, stop bool) {
	t.Helper()
	calls := len(f.calls)
	if stop {
		got, err := p.Stop(t.Context(), l, i)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("stop replay after removal: %+v %v", got, err)
		}
	}
	o, err := p.Abort(t.Context(), l, i)
	if err != nil || o.State != session.PlacementStopped || o.Stopped == nil || !reflect.DeepEqual(*o.Stopped, want) {
		t.Fatalf("abort replay after removal: %+v %v", o, err)
	}
	o, err = p.Inspect(t.Context(), l)
	if err != nil || o.State != session.PlacementStopped || o.Stopped == nil || !reflect.DeepEqual(*o.Stopped, want) {
		t.Fatalf("inspect after removal: %+v %v", o, err)
	}
	if len(f.calls) != calls {
		t.Fatalf("replay after removal used the engine: %v", f.calls[calls:])
	}
}

func TestStopRemovesVisitResourcesAndReplaysRetainedResult(t *testing.T) {
	dir := t.TempDir()
	p, f, a, l, i := activeFixture(t, dir)
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil || stopped.Receipt == nil || stopped.CheckpointDigest == "" {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
	assertReleased(t, p, f, l)
	assertAnsweredFromRecord(t, p, f, l, i, stopped, true)
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	p = openFixture(t, dir, f, a)
	assertAnsweredFromRecord(t, p, f, l, i, stopped, true)
	if err = p.Activate(t.Context(), l, i); err == nil {
		t.Fatal("removed visit reopened")
	}
}

func TestAbortRemovesVisitResourcesAndReplaysRetainedResult(t *testing.T) {
	p, f, a, l, i := activeFixture(t, t.TempDir())
	o, err := p.Abort(t.Context(), l, i)
	if err != nil || o.Stopped == nil || o.Stopped.Receipt == nil {
		t.Fatalf("abort: %+v %v", o, err)
	}
	assertReleased(t, p, f, l)
	assertAnsweredFromRecord(t, p, f, l, i, *o.Stopped, false)
	if a.seals != 1 {
		t.Fatalf("replay sealed again: %d", a.seals)
	}
	if _, err = p.Stop(t.Context(), l, i); err == nil {
		t.Fatal("aborted visit produced a move checkpoint")
	}
}

func TestMissingContainerWithoutRemovalRecordIsNotStopped(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		p, f, a, l, i := activeFixture(t, t.TempDir())
		delete(f.containers, i.ResourceUID)
		if o, err := p.Inspect(t.Context(), l); err == nil || o.State == session.PlacementStopped {
			t.Fatalf("missing writer inspected as stopped: %+v %v", o, err)
		}
		if o, err := p.Abort(t.Context(), l, i); err == nil || o.Stopped != nil {
			t.Fatalf("missing writer aborted as stopped: %+v %v", o, err)
		}
		r, err := p.load(l)
		if err != nil || r.ActualStopped || r.Removing || a.seals != 0 {
			t.Fatalf("missing writer recorded as fenced: %+v %v", r, err)
		}
	})
	t.Run("unknown-usage", func(t *testing.T) {
		p, f, a, l, i := activeFixture(t, t.TempDir())
		a.sealFail = true
		o, err := p.Abort(t.Context(), l, i)
		if err != nil || o.Stopped == nil || o.Stopped.Receipt != nil {
			t.Fatalf("abort with unknown usage: %+v %v", o, err)
		}
		// Unknown usage keeps the containers for a later sealing retry.
		if f.containers[i.ResourceUID] == nil || len(f.volumes) != 1 {
			t.Fatal("containers removed before usage was sealed")
		}
		delete(f.containers, i.ResourceUID)
		a.sealFail = false
		if _, err = p.Abort(t.Context(), l, i); err == nil {
			t.Fatal("externally removed writer accepted as stopped")
		}
		r, err := p.load(l)
		if err != nil || r.Removing || r.Receipt != nil {
			t.Fatalf("external removal changed the record: %+v %v", r, err)
		}
	})
}

func TestRemovalConvergesAfterEngineFailure(t *testing.T) {
	t.Run("stop", func(t *testing.T) {
		dir := t.TempDir()
		p, f, a, l, i := activeFixture(t, dir)
		f.failRemove = true
		stopped, err := p.Stop(t.Context(), l, i)
		if err != nil {
			t.Fatalf("removal failure changed the durable stop: %v", err)
		}
		r, err := p.load(l)
		if err != nil || !r.Removing || r.Removed || len(f.containers) != 3 {
			t.Fatalf("removal intent was not recorded first: %+v %v", r, err)
		}
		// A restart between recording and removal keeps the stop answerable.
		if err = p.Close(); err != nil {
			t.Fatal(err)
		}
		p = openFixture(t, dir, f, a)
		if o, err := p.Inspect(t.Context(), l); err != nil || o.Stopped == nil || !reflect.DeepEqual(*o.Stopped, stopped) {
			t.Fatalf("inspect before removal: %+v %v", o, err)
		}
		f.failRemove = false
		replay, err := p.Stop(t.Context(), l, i)
		if err != nil || !reflect.DeepEqual(replay, stopped) {
			t.Fatalf("stop retry: %+v %v", replay, err)
		}
		assertReleased(t, p, f, l)
		assertAnsweredFromRecord(t, p, f, l, i, stopped, true)
	})
	t.Run("partial", func(t *testing.T) {
		p, f, _, l, i := activeFixture(t, t.TempDir())
		original := p.engine.execute
		p.engine.execute = func(ctx context.Context, input []byte, max int, args []string) ([]byte, error) {
			if len(args) > 2 && args[0] == containerFixtureCommand && args[1] == "rm" && args[2] != i.ResourceUID {
				return nil, errors.New("engine removal interrupted")
			}
			return original(ctx, input, max, args)
		}
		stopped, err := p.Stop(t.Context(), l, i)
		if err != nil {
			t.Fatal(err)
		}
		if f.containers[i.ResourceUID] != nil || len(f.containers) != 2 {
			t.Fatal("expected only the agent to be removed")
		}
		p.engine.execute = original
		// The record, not the absence, proves the agent was removed here.
		o, err := p.Abort(t.Context(), l, i)
		if err != nil || o.Stopped == nil || !reflect.DeepEqual(*o.Stopped, stopped) {
			t.Fatalf("abort retry: %+v %v", o, err)
		}
		assertReleased(t, p, f, l)
	})
}

func TestCancelledPreparationRemovesCreatedResources(t *testing.T) {
	f := newFakeDocker()
	a := &fakeAuthority{engine: f}
	p := openFixture(t, t.TempDir(), f, a)
	original := p.engine.execute
	p.engine.execute = func(ctx context.Context, input []byte, max int, args []string) ([]byte, error) {
		if args[0] == createCommand && strings.HasSuffix(args[2], "-"+roleAgent) {
			return nil, errors.New("agent image unavailable")
		}
		return original(ctx, input, max, args)
	}
	l := fixtureLaunch(t, fixtureArtifact(t))
	l.ThreadID, l.CheckpointDigest = "", ""
	if _, err := p.Prepare(t.Context(), l, []byte("workspace-continuity")); err == nil {
		t.Fatal("expected failed preparation")
	}
	if err := p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	assertReleasedPreparation(t, p, f, l)
	calls := len(f.calls)
	if err := p.AbortPreparation(t.Context(), l); err != nil || len(f.calls) != calls {
		t.Fatalf("cancellation replay after removal: %v", err)
	}
	if _, err := p.Prepare(t.Context(), l, []byte("workspace-continuity")); err == nil {
		t.Fatal("cancelled preparation reopened")
	}
}

func assertReleasedPreparation(t *testing.T, p *Placement, f *fakeDocker, l session.Launch) {
	t.Helper()
	if len(f.containers) != 0 || len(f.volumes) != 0 {
		t.Fatalf("cancelled preparation left resources: %d containers, %d volumes", len(f.containers), len(f.volumes))
	}
	r, err := p.load(l)
	if err != nil || !r.ActualStopped || !r.Removed || r.Receipt != nil {
		t.Fatalf("cancelled preparation record: %+v %v", r, err)
	}
}
