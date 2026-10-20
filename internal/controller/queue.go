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

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

// IsActive reports whether a run holds admission capacity: it is Admitted or
// Running.
func IsActive(phase sprooziv1alpha1.AgentRunPhase) bool {
	return phase == sprooziv1alpha1.AgentRunPhaseAdmitted || phase == sprooziv1alpha1.AgentRunPhaseRunning
}

// ActiveRuns returns the AgentRuns in Admitted or Running phase. The caller
// passes a cluster-wide list, so the result counts against the cluster cap.
func ActiveRuns(runs []sprooziv1alpha1.AgentRun) []*sprooziv1alpha1.AgentRun {
	var active []*sprooziv1alpha1.AgentRun
	for i := range runs {
		if IsActive(runs[i].Status.Phase) {
			active = append(active, &runs[i])
		}
	}
	return active
}

// QueuedRuns returns the Queued AgentRuns first-in, first-out: earliest
// creation timestamp first, breaking ties by namespace then name (lexical
// ascending). The cluster-wide sort includes namespace so the ordering is
// stable when equal timestamps appear across different namespaces. A run not
// yet initialized (empty phase) keeps its place, so a later run cannot
// overtake it while its first status write is pending.
func QueuedRuns(runs []sprooziv1alpha1.AgentRun) []*sprooziv1alpha1.AgentRun {
	queued := make([]*sprooziv1alpha1.AgentRun, 0, len(runs))
	for i := range runs {
		if phase := runs[i].Status.Phase; phase == sprooziv1alpha1.AgentRunPhaseQueued || phase == "" {
			queued = append(queued, &runs[i])
		}
	}
	slices.SortFunc(queued, func(a, b *sprooziv1alpha1.AgentRun) int {
		ta := a.CreationTimestamp.Time
		tb := b.CreationTimestamp.Time
		if !ta.Equal(tb) {
			if ta.Before(tb) {
				return -1
			}
			return 1
		}
		if r := cmp.Compare(a.Namespace, b.Namespace); r != 0 {
			return r
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return queued
}

// AdmissionCandidate is a queued run in first-in, first-out order with the
// AgentPolicy it would be admitted under. Unresolved candidates, whose
// template or policy cannot be read, are skipped by the simulation.
type AdmissionCandidate struct {
	Run       types.NamespacedName
	Policy    types.NamespacedName
	PolicyCap int32 // 0 means no per-policy limit.
	Resolved  bool
}

// AdmissionState is the capacity already used by active runs.
type AdmissionState struct {
	GlobalCap    int // 0 means no cluster-wide limit.
	GlobalActive int
	PolicyActive map[types.NamespacedName]int
}

// AdmissionDecision says whether the target run is admitted now. When it is
// not, Message names what is full.
type AdmissionDecision struct {
	Admit   bool
	Message string
}

// SimulateAdmission walks the queue in order and admits each resolved
// candidate while the cluster cap and its policy's cap have room. A candidate
// whose policy is full is skipped, so it does not block later candidates under
// other policies. The target is admitted only if the simulation admits it.
func SimulateAdmission(state AdmissionState, queue []AdmissionCandidate, target types.NamespacedName) AdmissionDecision {
	globalUsed := state.GlobalActive
	policyUsed := make(map[types.NamespacedName]int, len(state.PolicyActive))
	maps.Copy(policyUsed, state.PolicyActive)
	for _, c := range queue {
		if !c.Resolved {
			continue
		}
		globalFull := state.GlobalCap > 0 && globalUsed >= state.GlobalCap
		policyFull := c.PolicyCap > 0 && policyUsed[c.Policy] >= int(c.PolicyCap)
		if c.Run == target {
			if !globalFull && !policyFull {
				return AdmissionDecision{Admit: true}
			}
			return AdmissionDecision{Message: capMessage(state, c, policyFull, globalFull)}
		}
		if !globalFull && !policyFull {
			globalUsed++
			policyUsed[c.Policy]++
		}
	}
	return AdmissionDecision{Message: "AgentRun is not yet in the admission queue"}
}

func capMessage(state AdmissionState, c AdmissionCandidate, policyFull, globalFull bool) string {
	var full []string
	earlier := false
	if policyFull {
		full = append(full, fmt.Sprintf("policy cap %d (AgentPolicy %s)", c.PolicyCap, c.Policy.Name))
		earlier = earlier || state.PolicyActive[c.Policy] < int(c.PolicyCap)
	}
	if globalFull {
		full = append(full, fmt.Sprintf("cluster cap %d", state.GlobalCap))
		earlier = earlier || state.GlobalActive < state.GlobalCap
	}
	message := "AgentRun is held by a concurrency cap: " + strings.Join(full, " and ") + " reached"
	if earlier {
		message += "; waiting for earlier queued runs"
	}
	return message
}
