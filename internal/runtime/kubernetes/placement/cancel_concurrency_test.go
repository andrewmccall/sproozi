package placement

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/google/uuid"
)

func TestAbortIndependentAllocationWhileAnotherStopIsHeld(t *testing.T) {
	p, _, _, first, data := setup(t)
	second := first
	second.ExecutionID, second.OperationID, second.AllocationID = uuid.NewString(), uuid.NewString(), uuid.NewString()
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
	_, r, err := p.retained(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	pod, err := p.pod(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	held := &heldQuiesceExecutor{commandExecutor: p.exec, pod: pod.Name, entered: make(chan struct{})}
	p.exec = held
	ctx, release := context.WithCancel(t.Context())
	defer release()
	stopDone := make(chan error, 1)
	go func() { _, err := p.Stop(ctx, first, firstInc); stopDone <- err }()
	awaitAllocationSignal(t, held.entered)
	abortDone := make(chan error, 1)
	go func() {
		o, err := p.Abort(t.Context(), second, secondInc)
		if err == nil && (o.State != session.PlacementStopped || o.Stopped == nil || !o.Stopped.ActualStopped) {
			err = errors.New("second allocation was not fenced")
		}
		abortDone <- err
	}()
	if err = awaitAllocationResult(t, abortDone); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-stopDone:
		t.Fatalf("second allocation interrupted first native turn: %v", err)
	default:
	}
	r, err = p.registry.Get(t.Context(), r.Reference)
	if err != nil || r.Phase != registry.Active {
		t.Fatalf("first source's active-turn grants were revoked: %+v %v", r, err)
	}
	pod, err = p.pod(t.Context(), r)
	if err != nil || !running(pod, nativeContainer) {
		t.Fatalf("first source's actual writer was stopped: %+v %v", pod, err)
	}
	release()
	if err = awaitAllocationResult(t, stopDone); err == nil {
		t.Fatal("interrupted first stop was claimed complete")
	}
}

func TestConcurrentPreparationsCannotReplaceSessionEpoch(t *testing.T) {
	p, kube, _, first, data := setup(t)
	second := first
	second.OperationID, second.AllocationID = uuid.NewString(), uuid.NewString()
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, l := range []session.Launch{first, second} {
		go func() { <-start; _, err := p.Prepare(t.Context(), l, data); results <- err }()
	}
	close(start)
	accepted, denied := 0, 0
	for range 2 {
		err := awaitAllocationResult(t, results)
		if err == nil {
			accepted++
			continue
		}
		var rejection *profile.Rejection
		if !errors.As(err, &rejection) || rejection.Code != profile.CodeConflict {
			t.Fatalf("unexpected preparation failure: %v", err)
		}
		denied++
	}
	if accepted != 1 || denied != 1 || kube.runCreates != 1 {
		t.Fatalf("epoch admitted alternative allocations: accepted=%d denied=%d Runs=%d", accepted, denied, kube.runCreates)
	}
}

type heldQuiesceExecutor struct {
	commandExecutor
	pod     string
	entered chan struct{}
}

func (e *heldQuiesceExecutor) Execute(ctx context.Context, namespace, pod, container string, args []string, input io.Reader, output, errout io.Writer) error {
	if pod == e.pod && len(args) == 2 && args[1] == string(harness.NativeQuiesce) {
		close(e.entered)
		<-ctx.Done()
		return ctx.Err()
	}
	return e.commandExecutor.Execute(ctx, namespace, pod, container, args, input, output, errout)
}

func awaitAllocationSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("allocation test signal did not arrive")
	}
}

func awaitAllocationResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("allocation operation did not return")
		return nil
	}
}
