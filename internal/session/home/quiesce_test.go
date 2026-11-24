package home

import (
	"context"
	"testing"
	"testing/synctest"
)

// A control handler goes on running after it has replied: an attach replies,
// then opens the terminal. The service's clean-up must wait for it, or it
// writes into the test's directory while that directory is removed.
func TestCleanUpWaitsForAHandlerThatOutlivesItsReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, local, _, repo := testService(t)
		id := startedOn(t, s, repo, testLocal)
		entered, release := make(chan struct{}), make(chan struct{})
		local.attach = func(context.Context) { close(entered); <-release }
		exchanged, quiet := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(exchanged)
			exchange(t, s, Request{Action: ActionAttach, ID: id})
		}()
		<-entered
		<-exchanged
		go func() { quiesce(s); close(quiet) }()
		synctest.Wait()
		select {
		case <-quiet:
			t.Fatal("clean-up returned while the control handler was still running")
		default:
		}
		close(release)
		<-quiet
	})
}
