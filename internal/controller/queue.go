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
	"slices"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

// ActiveRun returns the first AgentRun in Admitted or Running phase across
// all namespaces, or nil if none exist. The controller enforces a single
// global active-run slot by querying without a namespace filter.
func ActiveRun(runs []sprooziv1alpha1.AgentRun) *sprooziv1alpha1.AgentRun {
	for i := range runs {
		phase := runs[i].Status.Phase
		if phase == sprooziv1alpha1.AgentRunPhaseAdmitted || phase == sprooziv1alpha1.AgentRunPhaseRunning {
			return &runs[i]
		}
	}
	return nil
}

// NextQueued returns the Queued AgentRun with the earliest creation timestamp,
// breaking ties by namespace then name (lexical ascending). The cluster-wide
// sort includes namespace so the ordering is stable when equal timestamps
// appear across different namespaces. Returns nil if no Queued runs exist.
func NextQueued(runs []sprooziv1alpha1.AgentRun) *sprooziv1alpha1.AgentRun {
	queued := make([]*sprooziv1alpha1.AgentRun, 0, len(runs))
	for i := range runs {
		if runs[i].Status.Phase == sprooziv1alpha1.AgentRunPhaseQueued {
			queued = append(queued, &runs[i])
		}
	}
	if len(queued) == 0 {
		return nil
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
	return queued[0]
}
