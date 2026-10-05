/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller_test

import (
	"testing"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/controller"
)

func TestIsTerminal(t *testing.T) {
	t.Parallel()

	terminalPhases := []sprooziv1alpha1.AgentRunPhase{
		sprooziv1alpha1.AgentRunPhaseSucceeded,
		sprooziv1alpha1.AgentRunPhaseFailed,
		sprooziv1alpha1.AgentRunPhaseTimedOut,
		sprooziv1alpha1.AgentRunPhaseCancelled,
	}
	activePhases := []sprooziv1alpha1.AgentRunPhase{
		"",
		sprooziv1alpha1.AgentRunPhaseQueued,
		sprooziv1alpha1.AgentRunPhaseAdmitted,
		sprooziv1alpha1.AgentRunPhaseRunning,
	}

	for _, phase := range terminalPhases {
		if !controller.IsTerminal(phase) {
			t.Errorf("IsTerminal(%q) = false, want true", phase)
		}
	}
	for _, phase := range activePhases {
		if controller.IsTerminal(phase) {
			t.Errorf("IsTerminal(%q) = true, want false", phase)
		}
	}
}

func TestValidTransition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		from  sprooziv1alpha1.AgentRunPhase
		to    sprooziv1alpha1.AgentRunPhase
		valid bool
	}{
		// Valid: initial assignment
		{"", sprooziv1alpha1.AgentRunPhaseQueued, true},
		// Valid: admission path
		{sprooziv1alpha1.AgentRunPhaseQueued, sprooziv1alpha1.AgentRunPhaseAdmitted, true},
		{sprooziv1alpha1.AgentRunPhaseQueued, sprooziv1alpha1.AgentRunPhaseFailed, true},
		{sprooziv1alpha1.AgentRunPhaseQueued, sprooziv1alpha1.AgentRunPhaseCancelled, true},
		// Valid: execution path
		{sprooziv1alpha1.AgentRunPhaseAdmitted, sprooziv1alpha1.AgentRunPhaseRunning, true},
		{sprooziv1alpha1.AgentRunPhaseAdmitted, sprooziv1alpha1.AgentRunPhaseFailed, true},
		{sprooziv1alpha1.AgentRunPhaseAdmitted, sprooziv1alpha1.AgentRunPhaseCancelled, true},
		{sprooziv1alpha1.AgentRunPhaseAdmitted, sprooziv1alpha1.AgentRunPhaseTimedOut, true},
		// Valid: terminal transitions
		{sprooziv1alpha1.AgentRunPhaseRunning, sprooziv1alpha1.AgentRunPhaseSucceeded, true},
		{sprooziv1alpha1.AgentRunPhaseRunning, sprooziv1alpha1.AgentRunPhaseFailed, true},
		{sprooziv1alpha1.AgentRunPhaseRunning, sprooziv1alpha1.AgentRunPhaseTimedOut, true},
		{sprooziv1alpha1.AgentRunPhaseRunning, sprooziv1alpha1.AgentRunPhaseCancelled, true},
		// Invalid: skip steps
		{sprooziv1alpha1.AgentRunPhaseQueued, sprooziv1alpha1.AgentRunPhaseRunning, false},
		{sprooziv1alpha1.AgentRunPhaseQueued, sprooziv1alpha1.AgentRunPhaseSucceeded, false},
		{sprooziv1alpha1.AgentRunPhaseQueued, sprooziv1alpha1.AgentRunPhaseTimedOut, false},
		{"", sprooziv1alpha1.AgentRunPhaseAdmitted, false},
		{"", sprooziv1alpha1.AgentRunPhaseRunning, false},
		// Invalid: from terminal states
		{sprooziv1alpha1.AgentRunPhaseSucceeded, sprooziv1alpha1.AgentRunPhaseQueued, false},
		{sprooziv1alpha1.AgentRunPhaseSucceeded, sprooziv1alpha1.AgentRunPhaseRunning, false},
		{sprooziv1alpha1.AgentRunPhaseFailed, sprooziv1alpha1.AgentRunPhaseQueued, false},
		{sprooziv1alpha1.AgentRunPhaseTimedOut, sprooziv1alpha1.AgentRunPhaseRunning, false},
		{sprooziv1alpha1.AgentRunPhaseCancelled, sprooziv1alpha1.AgentRunPhaseQueued, false},
		// Invalid: back-transitions
		{sprooziv1alpha1.AgentRunPhaseAdmitted, sprooziv1alpha1.AgentRunPhaseQueued, false},
		{sprooziv1alpha1.AgentRunPhaseRunning, sprooziv1alpha1.AgentRunPhaseQueued, false},
		{sprooziv1alpha1.AgentRunPhaseRunning, sprooziv1alpha1.AgentRunPhaseAdmitted, false},
		// Invalid: same-state transition (not a valid forward move)
		{sprooziv1alpha1.AgentRunPhaseQueued, sprooziv1alpha1.AgentRunPhaseQueued, false},
	}

	for _, tt := range tests {
		got := controller.ValidTransition(tt.from, tt.to)
		if got != tt.valid {
			t.Errorf("ValidTransition(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.valid)
		}
	}
}
