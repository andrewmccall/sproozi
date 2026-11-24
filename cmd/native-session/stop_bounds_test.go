package main

import (
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// A remote executor sizes how long it carries a stop on, and how long home
// waits for it, from the bounds harness publishes. The supervisor's own
// bounds must be those, or a stop could outlast the executor's bound and
// lose its reply.
func TestSupervisorStopBoundsAreThePublishedOnes(t *testing.T) {
	if quiesceWait != harness.NativeQuiesceWait || quitWait != harness.NativeQuitWait ||
		forcedStopWait != harness.NativeForcedStopWait || quiesceReplyGrace != harness.NativeQuiesceReplyGrace {
		t.Fatalf("supervisor bounds: quiesce %s, quit %s, forced stop %s, reply grace %s; published %s, %s, %s, %s",
			quiesceWait, quitWait, forcedStopWait, quiesceReplyGrace, harness.NativeQuiesceWait,
			harness.NativeQuitWait, harness.NativeForcedStopWait, harness.NativeQuiesceReplyGrace)
	}
}
