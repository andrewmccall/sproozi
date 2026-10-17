package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCancelInterruptsHeldMove(t *testing.T) {
	c, _, local, remote, start := setup(t)
	if _, err := c.Move(t.Context(), start.Source.SessionID, uuid.NewString(), testRemote); err != nil {
		t.Fatal(err)
	}
	held := &heldStopPlacement{Placement: remote, entered: make(chan struct{}), aborted: make(chan struct{})}
	c.placements[testRemote] = held
	moveCtx, releaseMove := context.WithCancel(t.Context())
	defer releaseMove()
	moveDone := make(chan error, 1)
	go func() {
		_, err := c.Move(moveCtx, start.Source.SessionID, uuid.NewString(), testLocal)
		moveDone <- err
	}()
	awaitResponsivenessSignal(t, held.entered)
	cancelDone := make(chan error, 1)
	go func() {
		_, err := c.Cancel(t.Context(), start.Source.SessionID, uuid.NewString())
		cancelDone <- err
	}()
	// Cancel must release the graceful operation itself, without the caller
	// cancelling Move, disconnecting an attachment, or reaching the deadline.
	if err := awaitResponsivenessResult(t, moveDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("held move: %v", err)
	}
	if err := awaitResponsivenessResult(t, cancelDone); err != nil {
		t.Fatal(err)
	}
	awaitResponsivenessSignal(t, held.aborted)
	r, err := c.Inspect(start.Source.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != PhaseUnknownUsage || r.CancelOperation == "" || local.prepared != 0 || remote.aborted != 1 {
		t.Fatalf("forced cancellation or source-only fencing changed: %+v", r)
	}
	if r.Checkpoint.Metadata.SHA256 != start.Stopped.CheckpointDigest || r.Remaining[testCapability].Units.Value != 18 {
		t.Fatal("cancel replaced the last committed checkpoint or refunded unknown usage")
	}
	if _, err = c.Cancel(t.Context(), r.ID, r.CancelOperation); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Cancel(t.Context(), r.ID, uuid.NewString()); err == nil {
		t.Fatal("retry changed retained cancellation identity")
	}
}

type heldStopPlacement struct {
	Placement
	entered, aborted chan struct{}
	abortOnce        sync.Once
	completeOnCancel bool
}

func (p *heldStopPlacement) Stop(ctx context.Context, l Launch, i Incarnation) (Stopped, error) {
	close(p.entered)
	<-ctx.Done()
	if p.completeOnCancel {
		return p.Placement.Stop(context.WithoutCancel(ctx), l, i)
	}
	return Stopped{}, ctx.Err()
}

func (p *heldStopPlacement) Abort(ctx context.Context, l Launch, i Incarnation) (Observation, error) {
	p.abortOnce.Do(func() { close(p.aborted) })
	return p.Placement.Abort(ctx, l, i)
}

func TestCancelDoesNotInterruptUnrelatedSession(t *testing.T) {
	c, _, _, remote, start := setup(t)
	if _, err := c.Move(t.Context(), start.Source.SessionID, uuid.NewString(), testRemote); err != nil {
		t.Fatal(err)
	}
	held := &heldStopPlacement{Placement: remote, entered: make(chan struct{}), aborted: make(chan struct{})}
	c.placements[testRemote] = held
	ctx, release := context.WithCancel(t.Context())
	defer release()
	moveDone := make(chan error, 1)
	go func() {
		_, err := c.Move(ctx, start.Source.SessionID, uuid.NewString(), testLocal)
		moveDone <- err
	}()
	awaitResponsivenessSignal(t, held.entered)
	if _, err := c.Cancel(t.Context(), start.Source.SessionID, "not-a-uuid"); err == nil {
		t.Fatal("invalid cancellation ID accepted")
	}
	unrelated := make(chan error, 1)
	go func() {
		_, err := c.Cancel(t.Context(), uuid.NewString(), uuid.NewString())
		unrelated <- err
	}()
	select {
	case err := <-moveDone:
		t.Fatalf("different session interrupted the held move: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancelDone := make(chan error, 1)
	go func() {
		_, err := c.Cancel(t.Context(), start.Source.SessionID, uuid.NewString())
		cancelDone <- err
	}()
	if err := awaitResponsivenessResult(t, cancelDone); err != nil {
		t.Fatal(err)
	}
	if err := awaitResponsivenessResult(t, moveDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("held move: %v", err)
	}
	if err := awaitResponsivenessResult(t, unrelated); err == nil {
		t.Fatal("nonexistent unrelated session was cancelled")
	}
}

func TestCancelIndependentSessionWhileAnotherMoveIsHeld(t *testing.T) {
	c, _, _, remote, start := setup(t)
	other := start
	other.Source.SessionID = uuid.NewString()
	other.Source.OperationID = uuid.NewString()
	other.Source.AllocationID = uuid.NewString()
	other.Stopped.Launch = other.Source
	other.Stopped.Receipt = &Receipt{Launch: other.Source, Incarnation: other.Incarnation, Usage: start.Stopped.Receipt.Usage}
	if _, err := c.Create(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{start.Source.SessionID, other.Source.SessionID} {
		if _, err := c.Move(t.Context(), id, uuid.NewString(), testRemote); err != nil {
			t.Fatal(err)
		}
	}
	held := &heldStopPlacement{Placement: remote, entered: make(chan struct{}), aborted: make(chan struct{})}
	c.placements[testRemote] = held
	ctx, release := context.WithCancel(t.Context())
	defer release()
	moveDone := make(chan error, 1)
	go func() {
		_, err := c.Move(ctx, start.Source.SessionID, uuid.NewString(), testLocal)
		moveDone <- err
	}()
	awaitResponsivenessSignal(t, held.entered)
	cancelDone := make(chan error, 1)
	go func() {
		_, err := c.Cancel(t.Context(), other.Source.SessionID, uuid.NewString())
		cancelDone <- err
	}()
	if err := awaitResponsivenessResult(t, cancelDone); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-moveDone:
		t.Fatalf("cancelling B interrupted A's completed-turn wait: %v", err)
	default:
	}
	r, err := c.Inspect(other.Source.SessionID)
	if err != nil || r.CancelOperation == "" || r.Phase != PhaseUnknownUsage || remote.aborted != 1 {
		t.Fatalf("independent cancellation: %+v %v", r, err)
	}
	// A remains graceful; only cleanup cancels its caller context after B's
	// independent cancellation has already completed and been observed.
	release()
	if err := awaitResponsivenessResult(t, moveDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("cleanup of held A: %v", err)
	}
}

func TestCancelInterruptsHeldRecovery(t *testing.T) {
	c, _, local, remote, start := setup(t)
	if _, err := c.Move(t.Context(), start.Source.SessionID, uuid.NewString(), testRemote); err != nil {
		t.Fatal(err)
	}
	remote.unknownStop = true
	if _, err := c.Move(t.Context(), start.Source.SessionID, uuid.NewString(), testLocal); err == nil {
		t.Fatal("failed stop did not leave a recovery intent")
	}
	remote.unknownStop = false
	held := &heldStopPlacement{Placement: remote, entered: make(chan struct{}), aborted: make(chan struct{})}
	c.placements[testRemote] = held
	ctx, release := context.WithCancel(t.Context())
	defer release()
	recoveryDone := make(chan error, 1)
	go func() {
		_, err := c.Recover(ctx, start.Source.SessionID)
		recoveryDone <- err
	}()
	awaitResponsivenessSignal(t, held.entered)
	cancelDone := make(chan error, 1)
	go func() {
		_, err := c.Cancel(t.Context(), start.Source.SessionID, uuid.NewString())
		cancelDone <- err
	}()
	if err := awaitResponsivenessResult(t, cancelDone); err != nil {
		t.Fatal(err)
	}
	if err := awaitResponsivenessResult(t, recoveryDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("held recovery: %v", err)
	}
	if local.prepared != 0 || remote.aborted != 1 {
		t.Fatal("recovery started a destination after forced cancellation")
	}
}

func TestCancelPreventsActivationAfterRetainedInspectReply(t *testing.T) {
	c, _, _, remote, start := setup(t)
	held := &heldPreparedInspect{Placement: remote, entered: make(chan struct{})}
	c.placements[testRemote] = held
	ctx, release := context.WithCancel(t.Context())
	defer release()
	moveDone := make(chan error, 1)
	go func() {
		_, err := c.Move(ctx, start.Source.SessionID, uuid.NewString(), testRemote)
		moveDone <- err
	}()
	awaitResponsivenessSignal(t, held.entered)
	cancelDone := make(chan error, 1)
	go func() {
		_, err := c.Cancel(t.Context(), start.Source.SessionID, uuid.NewString())
		cancelDone <- err
	}()
	if err := awaitResponsivenessResult(t, cancelDone); err != nil {
		t.Fatal(err)
	}
	if err := awaitResponsivenessResult(t, moveDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("move accepted a retained response after cancellation: %v", err)
	}
	if remote.prepared != 1 || remote.activated != 0 || remote.aborted != 1 {
		t.Fatalf("inert destination was activated or lost: %+v", remote)
	}
}

func TestCancelCommitsCompletedSourceWithoutPreparingDestination(t *testing.T) {
	c, _, local, remote, start := setup(t)
	if _, err := c.Move(t.Context(), start.Source.SessionID, uuid.NewString(), testRemote); err != nil {
		t.Fatal(err)
	}
	held := &heldStopPlacement{Placement: remote, entered: make(chan struct{}), aborted: make(chan struct{}), completeOnCancel: true}
	c.placements[testRemote] = held
	ctx, release := context.WithCancel(t.Context())
	defer release()
	moveDone := make(chan error, 1)
	go func() {
		_, err := c.Move(ctx, start.Source.SessionID, uuid.NewString(), testLocal)
		moveDone <- err
	}()
	awaitResponsivenessSignal(t, held.entered)
	cancelDone := make(chan error, 1)
	go func() {
		_, err := c.Cancel(t.Context(), start.Source.SessionID, uuid.NewString())
		cancelDone <- err
	}()
	if err := awaitResponsivenessResult(t, cancelDone); err != nil {
		t.Fatal(err)
	}
	if err := awaitResponsivenessResult(t, moveDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("completed stop response bypassed cancellation: %v", err)
	}
	r, err := c.Inspect(start.Source.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != PhaseCancelled || len(r.Visits) != 2 || r.Remaining[testCapability].Units.Value != 13 || r.Active != nil {
		t.Fatalf("genuine completed receipt was discarded: %+v", r)
	}
	if local.prepared != 0 || local.activated != 0 || remote.stopped != 1 || remote.aborted != 0 {
		t.Fatal("cancellation prepared another writer or unnecessarily aborted a sealed stopped source")
	}
}

type heldPreparedInspect struct {
	Placement
	entered chan struct{}
	blocked bool
}

func (p *heldPreparedInspect) Inspect(ctx context.Context, l Launch) (Observation, error) {
	o, err := p.Placement.Inspect(ctx, l)
	if err == nil && o.State == PlacementPrepared && !p.blocked {
		p.blocked = true
		close(p.entered)
		<-ctx.Done()
		// Retained evidence is still true even when its request was interrupted.
		// The coordinator must check before issuing a new activation side effect.
	}
	return o, err
}

func awaitResponsivenessSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle test signal did not arrive")
	}
}

func awaitResponsivenessResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle test operation did not return")
		return nil
	}
}
