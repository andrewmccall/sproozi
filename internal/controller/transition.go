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

package controller

import sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"

// IsTerminal reports whether phase is a terminal lifecycle state from which no
// further transitions are permitted.
func IsTerminal(phase sprooziv1alpha1.AgentRunPhase) bool {
	switch phase {
	case sprooziv1alpha1.AgentRunPhaseSucceeded,
		sprooziv1alpha1.AgentRunPhaseFailed,
		sprooziv1alpha1.AgentRunPhaseTimedOut,
		sprooziv1alpha1.AgentRunPhaseCancelled:
		return true
	}
	return false
}

// ValidTransition reports whether transitioning from the given phase to the
// target phase is a permitted lifecycle step.
func ValidTransition(from, to sprooziv1alpha1.AgentRunPhase) bool {
	switch from {
	case "":
		return to == sprooziv1alpha1.AgentRunPhaseQueued
	case sprooziv1alpha1.AgentRunPhaseQueued:
		switch to {
		case sprooziv1alpha1.AgentRunPhaseAdmitted,
			sprooziv1alpha1.AgentRunPhaseFailed,
			sprooziv1alpha1.AgentRunPhaseCancelled:
			return true
		}
	case sprooziv1alpha1.AgentRunPhaseAdmitted:
		switch to {
		case sprooziv1alpha1.AgentRunPhaseRunning,
			sprooziv1alpha1.AgentRunPhaseFailed,
			sprooziv1alpha1.AgentRunPhaseTimedOut,
			sprooziv1alpha1.AgentRunPhaseCancelled:
			return true
		}
	case sprooziv1alpha1.AgentRunPhaseRunning:
		switch to {
		case sprooziv1alpha1.AgentRunPhaseSucceeded,
			sprooziv1alpha1.AgentRunPhaseFailed,
			sprooziv1alpha1.AgentRunPhaseTimedOut,
			sprooziv1alpha1.AgentRunPhaseCancelled:
			return true
		}
	}
	return false
}
