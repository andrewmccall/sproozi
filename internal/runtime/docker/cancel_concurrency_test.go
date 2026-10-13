package docker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
)

func TestAbortIndependentAllocationWhileAnotherStopIsHeld(t *testing.T) {
	f := newFakeDocker()
	a := &fakeAuthority{engine: f}
	p := openFixture(t, t.TempDir(), f, a)
	data := fixtureArtifact(t)
	first, second := fixtureLaunch(t, data), fixtureLaunch(t, data)
	var firstInc, secondInc session.Incarnation
	for index, l := range []session.Launch{first, second} {
		i, err := p.Prepare(t.Context(), l, data)
		if err != nil {
			t.Fatal(err)
		}
		if err = p.Activate(t.Context(), l, i); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			firstInc = i
		} else {
			secondInc = i
		}
	}
	entered := make(chan struct{})
	original := p.engine.execute
	p.engine.execute = func(ctx context.Context, input []byte, max int, args []string) ([]byte, error) {
		if len(args) == 5 && args[0] == execCommand && args[2] == firstInc.ResourceUID && args[3] == nativeCommandPath && args[4] == quiesceCommand {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return original(ctx, input, max, args)
	}
	ctx, release := context.WithCancel(t.Context())
	defer release()
	stopDone := make(chan error, 1)
	go func() { _, err := p.Stop(ctx, first, firstInc); stopDone <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first allocation did not enter graceful quiescence")
	}
	abortDone := make(chan error, 1)
	go func() {
		o, err := p.Abort(t.Context(), second, secondInc)
		if err == nil && (o.State != session.PlacementStopped || o.Stopped == nil || !o.Stopped.ActualStopped) {
			err = errors.New("second allocation was not fenced")
		}
		abortDone <- err
	}()
	select {
	case err := <-abortDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second allocation's forced abort waited behind first allocation")
	}
	select {
	case err := <-stopDone:
		t.Fatalf("independent abort interrupted the first allocation: %v", err)
	default:
	}
	if !f.containers[firstInc.ResourceUID].State.Running || f.containers[secondInc.ResourceUID].State.Running {
		t.Fatal("forced abort fenced the wrong actual container")
	}
	release()
	select {
	case err := <-stopDone:
		if err == nil {
			t.Fatal("interrupted graceful stop was claimed complete")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("graceful stop cleanup did not return")
	}
}
