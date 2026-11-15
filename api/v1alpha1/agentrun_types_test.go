package v1alpha1

import "testing"

func TestAgentRunPhaseIsTerminal(t *testing.T) {
	for _, phase := range []AgentRunPhase{AgentRunPhaseSucceeded, AgentRunPhaseFailed, AgentRunPhaseTimedOut,
		AgentRunPhaseCancelled} {
		if !phase.IsTerminal() {
			t.Errorf("%q.IsTerminal() = false, want true", phase)
		}
	}
	for _, phase := range []AgentRunPhase{"", AgentRunPhaseQueued, AgentRunPhaseAdmitted, AgentRunPhaseRunning} {
		if phase.IsTerminal() {
			t.Errorf("%q.IsTerminal() = true, want false", phase)
		}
	}
}
