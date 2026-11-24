package profile

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// The executor's own bound ends a stop whose request ended, so nothing it
// carries on lasts forever.
func TestStopWhoseRequestEndedIsBounded(t *testing.T) {
	t.Parallel()
	c, b, h, l, i, ended := heldFixture(t, 0)
	h.stopBound = 200 * time.Millisecond
	b.entered, b.release = make(chan struct{}), make(chan struct{})
	entered := b.entered
	ctx, leave := context.WithCancel(t.Context())
	first := make(chan error, 1)
	go func() {
		_, err := c.Stop(ctx, l, i)
		first <- err
	}()
	awaitSignal(t, entered, "the stop starting")
	leave()
	<-first
	awaitSignal(t, ended, "the executor noticing that home left")
	for range 200 {
		if ends := b.stopEnds(); len(ends) == 1 {
			if !errors.Is(ends[0], context.DeadlineExceeded) {
				t.Fatalf("the bounded stop ended with %v", ends[0])
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the executor carried the stop on past its bound")
}

// The client waits for each action at least as long as the executor may
// take, and the turn-pending answer arrives minutes before the client gives
// up.
func TestClientWaitsCoverTheExecutorsBounds(t *testing.T) {
	t.Parallel()
	c, err := Connect(Connection{Origin: "https://executor.example", Token: testToken, MaxCheckpointBytes: testMax})
	if err != nil {
		t.Fatal(err)
	}
	// A first visit's two graceful waits, the quit and kill waits after
	// them, and the supervisor's wait for its reply to be read.
	handoff := 2*harness.NativeQuiesceWait + 30*time.Second + 3*time.Second + 10*time.Second
	if stopHandoffBound < handoff {
		t.Fatalf("stop handoff bound %s is shorter than the supervisor's %s", stopHandoffBound, handoff)
	}
	if StopBound < stopHandoffBound+stopSettleBound || c.wait(actionStop) < StopBound+answerMargin {
		t.Fatalf("stop waits %s for an executor bound of %s", c.wait(actionStop), StopBound)
	}
	if latest := 2 * harness.NativeQuiesceWait; c.wait(actionStop)-latest < 5*time.Minute {
		t.Fatalf("a turn-pending answer at %s leaves too little of the stop's %s wait", latest, c.wait(actionStop))
	}
	if c.wait(actionPrepare) < interactivePodWait+answerMargin || c.wait(actionAbort) <= defaultTimeout ||
		c.wait(actionAbortPreparation) != abortWait {
		t.Fatalf("prepare waits %s, abort %s and abort-preparation %s", c.wait(actionPrepare), c.wait(actionAbort),
			c.wait(actionAbortPreparation))
	}
	for _, action := range []string{actionActivate, actionInspect, actionDescribe, actionAttach} {
		if c.wait(action) != 0 || c.http.Timeout != defaultTimeout {
			t.Fatalf("%s waits %s beyond the default %s", action, c.wait(action), c.http.Timeout)
		}
	}
	if ownerWait("stop") != stopWait || ownerWait("recover") != stopWait || ownerWait("cancel") != abortWait ||
		ownerWait("info") != 0 {
		t.Fatalf("owner waits: stop %s, recover %s, cancel %s, info %s", ownerWait("stop"), ownerWait("recover"),
			ownerWait("cancel"), ownerWait("info"))
	}
}
