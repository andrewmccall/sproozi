package home

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

// net.Pipe exercises the real control handler without privileged socket binds.
// The source holds an unfinished native turn; closing the Move client leaves its
// durable daemon operation running. An explicit Cancel then interrupts that
// daemon-owned operation and reaches Abort without waiting for the native turn.
func TestCancelInterruptsDisconnectedInitialMove(t *testing.T) {
	s, local, remote, repo := testService(t)
	id := uuid.NewString()
	if _, err := s.Start(t.Context(), id, uuid.NewString(), repo, "", nil); err != nil {
		t.Fatal(err)
	}
	entered, aborted := holdStop(local)
	moveCtx, releaseMove := context.WithCancel(t.Context())
	defer releaseMove()
	server, client := net.Pipe()
	defer func() { _ = server.Close(); _ = client.Close() }()
	moveDone := make(chan struct{})
	go func() { s.handle(moveCtx, server); close(moveDone) }()
	if err := json.NewEncoder(client).Encode(Request{Action: ActionMove, ID: id, OperationID: uuid.NewString(), Destination: testRemote}); err != nil {
		t.Fatal(err)
	}
	awaitHomeSignal(t, entered)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-moveDone:
		t.Fatal("disconnect unexpectedly cancelled durable Move")
	case <-time.After(100 * time.Millisecond):
	}
	cancelDone := make(chan error, 1)
	go func() {
		_, err := s.Cancel(t.Context(), id, uuid.NewString())
		cancelDone <- err
	}()
	awaitHomeSignal(t, moveDone)
	select {
	case err := <-cancelDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("forced cancellation did not interrupt the held first-visit move")
	}
	awaitHomeSignal(t, aborted)
	status, err := s.Status(id)
	if err != nil || status.Phase != string(session.PhaseCancelled) || local.aborts != 1 || remote.prepares != 0 {
		t.Fatalf("initial source cancellation: %+v %v", status, err)
	}
	if _, err = s.Cancel(t.Context(), id, status.Session.CancelOperation); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Cancel(t.Context(), id, uuid.NewString()); err == nil || local.aborts != 1 {
		t.Fatal("cancellation retry replaced initial intent or repeated the abort")
	}
}

func TestCancelInterruptsInitialRecovery(t *testing.T) {
	s, local, remote, repo := testService(t)
	id := uuid.NewString()
	if _, err := s.Start(t.Context(), id, uuid.NewString(), repo, "", nil); err != nil {
		t.Fatal(err)
	}
	// The failed Stop retains the first move operation; Recover must wait for the
	// same source's native turn and still allow a separate forced cancellation.
	local.lostStop = true
	if _, err := s.Move(t.Context(), id, uuid.NewString(), testRemote); err == nil {
		t.Fatal("lost stop reply did not retain initial recovery intent")
	}
	entered, _ := holdStop(local)
	ctx, release := context.WithCancel(t.Context())
	defer release()
	recoveryDone := make(chan error, 1)
	go func() {
		_, err := s.Recover(ctx, id)
		recoveryDone <- err
	}()
	awaitHomeSignal(t, entered)
	cancelDone := make(chan error, 1)
	go func() {
		_, err := s.Cancel(t.Context(), id, uuid.NewString())
		cancelDone <- err
	}()
	awaitHomeResult(t, recoveryDone, true)
	awaitHomeResult(t, cancelDone, false)
	if local.aborts != 1 || remote.prepares != 0 {
		t.Fatal("initial recovery activated a destination after forced cancellation")
	}
}

func TestCancelIndependentInitialSessionWhileAnotherMoveIsHeld(t *testing.T) {
	s, local, remote, repo := testService(t)
	heldID, cancelledID := uuid.NewString(), uuid.NewString()
	for _, id := range []string{heldID, cancelledID} {
		if _, err := s.Start(t.Context(), id, uuid.NewString(), repo, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	entered, _ := holdStop(local)
	ctx, release := context.WithCancel(t.Context())
	defer release()
	moveDone := make(chan error, 1)
	go func() {
		_, err := s.Move(ctx, heldID, uuid.NewString(), testRemote)
		moveDone <- err
	}()
	awaitHomeSignal(t, entered)
	cancelDone := make(chan error, 1)
	go func() {
		_, err := s.Cancel(t.Context(), cancelledID, uuid.NewString())
		cancelDone <- err
	}()
	awaitHomeResult(t, cancelDone, false)
	select {
	case err := <-moveDone:
		t.Fatalf("cancelling B interrupted A's graceful first-visit move: %v", err)
	default:
	}
	status, err := s.Status(cancelledID)
	if err != nil || status.Phase != string(session.PhaseCancelled) || local.aborts != 1 || remote.prepares != 0 {
		t.Fatalf("independent initial cancellation: %+v %v", status, err)
	}
	release()
	awaitHomeResult(t, moveDone, true)
}

func TestCancelPreventsInitialActivationAfterPreparedReply(t *testing.T) {
	s, local, _, repo := testService(t)
	local.holdPrepare = make(chan struct{})
	id := uuid.NewString()
	ctx, release := context.WithCancel(t.Context())
	defer release()
	operationID := uuid.NewString()
	// Capture runs real Git subprocesses and is setup for this test's prepared
	// reply boundary. Keep it outside the cancellation responsiveness guard;
	// Start must recover the same durable preparing intent without recapturing.
	if _, err := s.createWorkspaceExecution(ctx, id, operationID, repo, s.config.Local, nil); err != nil {
		t.Fatal(err)
	}
	startDone := make(chan error, 1)
	go func() {
		_, err := s.Start(ctx, id, operationID, repo, "", nil)
		startDone <- err
	}()
	select {
	case <-local.holdPrepare:
	case err := <-startDone:
		t.Fatalf("Start returned before the held preparation boundary: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("retained initial intent did not reach the preparation boundary")
	}
	cancelDone := make(chan error, 1)
	go func() {
		_, err := s.Cancel(t.Context(), id, uuid.NewString())
		cancelDone <- err
	}()
	awaitHomeResult(t, startDone, true)
	awaitHomeResult(t, cancelDone, false)
	status, err := s.Status(id)
	if err != nil || status.Phase != string(session.PhaseCancelled) || local.initials != 1 || local.activations != 0 || local.aborts != 1 {
		t.Fatalf("cancelled initial preparation activated: %+v %v", status, err)
	}
}

// holdStop makes the placement's next graceful stop wait for its context, as
// an unfinished native turn would. It returns channels closed when that stop
// and the next abort are reached.
func holdStop(p *placementFixture) (entered, aborted <-chan struct{}) {
	p.holdStop, p.abortEntered = make(chan struct{}), make(chan struct{})
	return p.holdStop, p.abortEntered
}

func awaitHomeSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle test signal did not arrive")
	}
}

func awaitHomeResult(t *testing.T, result <-chan error, failed bool) {
	t.Helper()
	select {
	case err := <-result:
		if (err != nil) != failed {
			t.Fatalf("unexpected lifecycle result: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle operation did not return")
	}
}
