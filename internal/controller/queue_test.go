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
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/controller"
)

func runWithPhaseNs(name, namespace string, phase sprooziv1alpha1.AgentRunPhase, createdAt time.Time) sprooziv1alpha1.AgentRun {
	return sprooziv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.Time{Time: createdAt},
		},
		Status: sprooziv1alpha1.AgentRunStatus{Phase: phase},
	}
}

func runWithPhase(name string, phase sprooziv1alpha1.AgentRunPhase, createdAt time.Time) sprooziv1alpha1.AgentRun {
	return runWithPhaseNs(name, "default", phase, createdAt)
}

func TestActiveRuns(t *testing.T) {
	t.Parallel()

	now := time.Now()

	tests := []struct {
		name      string
		runs      []sprooziv1alpha1.AgentRun
		wantNames []string
	}{
		{name: "empty list"},
		{
			name: "only queued runs",
			runs: []sprooziv1alpha1.AgentRun{runWithPhase("a", sprooziv1alpha1.AgentRunPhaseQueued, now)},
		},
		{
			name:      "admitted and running runs are active",
			runs:      []sprooziv1alpha1.AgentRun{runWithPhase("a", sprooziv1alpha1.AgentRunPhaseAdmitted, now), runWithPhase("b", sprooziv1alpha1.AgentRunPhaseRunning, now)},
			wantNames: []string{"a", "b"},
		},
		{
			name: "terminal runs are not active",
			runs: []sprooziv1alpha1.AgentRun{
				runWithPhase("a", sprooziv1alpha1.AgentRunPhaseSucceeded, now),
				runWithPhase("b", sprooziv1alpha1.AgentRunPhaseFailed, now),
				runWithPhase("c", sprooziv1alpha1.AgentRunPhaseCancelled, now),
				runWithPhase("d", sprooziv1alpha1.AgentRunPhaseTimedOut, now),
			},
		},
		{
			name: "every active run across namespaces among mixed phases",
			runs: []sprooziv1alpha1.AgentRun{
				runWithPhase("a", sprooziv1alpha1.AgentRunPhaseQueued, now),
				runWithPhase("b", sprooziv1alpha1.AgentRunPhaseAdmitted, now),
				runWithPhase("c", sprooziv1alpha1.AgentRunPhaseSucceeded, now),
				runWithPhaseNs("d", "team-b", sprooziv1alpha1.AgentRunPhaseRunning, now),
			},
			wantNames: []string{"b", "d"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			active := controller.ActiveRuns(tt.runs)
			got := make([]string, 0, len(active))
			for _, run := range active {
				got = append(got, run.Name)
			}
			if !slices.Equal(got, tt.wantNames) {
				t.Errorf("ActiveRuns() = %v, want %v", got, tt.wantNames)
			}
		})
	}
}

func TestQueuedRunsHead(t *testing.T) {
	t.Parallel()

	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Second)

	tests := []struct {
		name     string
		runs     []sprooziv1alpha1.AgentRun
		wantName string
	}{
		{
			name:     "empty list",
			runs:     nil,
			wantName: "",
		},
		{
			name:     "single queued run",
			runs:     []sprooziv1alpha1.AgentRun{runWithPhase("a", sprooziv1alpha1.AgentRunPhaseQueued, t1)},
			wantName: "a",
		},
		{
			name: "earliest creation timestamp wins",
			runs: []sprooziv1alpha1.AgentRun{
				runWithPhase("b", sprooziv1alpha1.AgentRunPhaseQueued, t2),
				runWithPhase("a", sprooziv1alpha1.AgentRunPhaseQueued, t1),
			},
			wantName: "a",
		},
		{
			name: "same timestamp, lexically first name wins",
			runs: []sprooziv1alpha1.AgentRun{
				runWithPhase("beta", sprooziv1alpha1.AgentRunPhaseQueued, t1),
				runWithPhase("alpha", sprooziv1alpha1.AgentRunPhaseQueued, t1),
			},
			wantName: "alpha",
		},
		{
			name: "same timestamp, namespace breaks tie before name",
			runs: []sprooziv1alpha1.AgentRun{
				runWithPhaseNs("a", "team-z", sprooziv1alpha1.AgentRunPhaseQueued, t1),
				runWithPhaseNs("a", "team-a", sprooziv1alpha1.AgentRunPhaseQueued, t1),
			},
			wantName: "a", // "team-a" namespace sorts first
		},
		{
			name: "only non-queued runs skipped",
			runs: []sprooziv1alpha1.AgentRun{
				runWithPhase("a", sprooziv1alpha1.AgentRunPhaseAdmitted, t1),
				runWithPhase("b", sprooziv1alpha1.AgentRunPhaseSucceeded, t1),
			},
			wantName: "",
		},
		{
			name: "earlier uninitialized run keeps its place",
			runs: []sprooziv1alpha1.AgentRun{
				runWithPhase("b", sprooziv1alpha1.AgentRunPhaseQueued, t2),
				runWithPhase("a", "", t1),
			},
			wantName: "a",
		},
		{
			name: "queued selected from mixed phases",
			runs: []sprooziv1alpha1.AgentRun{
				runWithPhase("a", sprooziv1alpha1.AgentRunPhaseSucceeded, t1),
				runWithPhase("b", sprooziv1alpha1.AgentRunPhaseQueued, t2),
				runWithPhase("c", sprooziv1alpha1.AgentRunPhaseAdmitted, t1),
			},
			wantName: "b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			queued := controller.QueuedRuns(tt.runs)
			if tt.wantName == "" {
				if len(queued) != 0 {
					t.Errorf("QueuedRuns()[0] = %q, want empty", queued[0].Name)
				}
			} else {
				if len(queued) == 0 {
					t.Fatalf("QueuedRuns() is empty, want head %q", tt.wantName)
				}
				if queued[0].Name != tt.wantName {
					t.Errorf("QueuedRuns()[0].Name = %q, want %q", queued[0].Name, tt.wantName)
				}
			}
		})
	}
}

func TestQueuedRunsOrdersWholeQueue(t *testing.T) {
	t.Parallel()

	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	runs := []sprooziv1alpha1.AgentRun{
		runWithPhaseNs("c", "team-a", sprooziv1alpha1.AgentRunPhaseQueued, t1.Add(time.Second)),
		runWithPhaseNs("b", "team-b", sprooziv1alpha1.AgentRunPhaseQueued, t1),
		runWithPhaseNs("a", "team-a", sprooziv1alpha1.AgentRunPhaseRunning, t1),
		runWithPhaseNs("z", "team-a", sprooziv1alpha1.AgentRunPhaseQueued, t1),
	}
	queued := controller.QueuedRuns(runs)
	got := make([]string, 0, len(queued))
	for _, run := range queued {
		got = append(got, run.Namespace+"/"+run.Name)
	}
	if want := []string{"team-a/z", "team-b/b", "team-a/c"}; !slices.Equal(got, want) {
		t.Fatalf("QueuedRuns() = %v, want %v", got, want)
	}
}

const targetRun = "run"

func TestSimulateAdmission(t *testing.T) {
	t.Parallel()

	key := func(name string) types.NamespacedName { return types.NamespacedName{Namespace: "default", Name: name} }
	polA, polB := key("policy-a"), key("policy-b")
	candidate := func(run string, policy types.NamespacedName, policyCap int32) controller.AdmissionCandidate {
		return controller.AdmissionCandidate{Run: key(run), Policy: policy, PolicyCap: policyCap, Resolved: true}
	}

	tests := []struct {
		name        string
		state       controller.AdmissionState
		queue       []controller.AdmissionCandidate
		target      string
		wantAdmit   bool
		wantMessage []string
	}{
		{
			name:      "no caps admits alongside active runs",
			state:     controller.AdmissionState{GlobalActive: 5, PolicyActive: map[types.NamespacedName]int{polA: 5}},
			queue:     []controller.AdmissionCandidate{candidate("first", polA, 0), candidate("second", polA, 0)},
			target:    "second",
			wantAdmit: true,
		},
		{
			name:        "cluster cap reached by active runs",
			state:       controller.AdmissionState{GlobalCap: 2, GlobalActive: 2},
			queue:       []controller.AdmissionCandidate{candidate(targetRun, polA, 0)},
			target:      targetRun,
			wantMessage: []string{"cluster cap 2"},
		},
		{
			name:        "cluster cap taken by an earlier queued run",
			state:       controller.AdmissionState{GlobalCap: 2, GlobalActive: 1},
			queue:       []controller.AdmissionCandidate{candidate("earlier", polA, 0), candidate("later", polB, 0)},
			target:      "later",
			wantMessage: []string{"cluster cap 2", "waiting for earlier queued runs"},
		},
		{
			name:        "policy cap reached",
			state:       controller.AdmissionState{GlobalActive: 1, PolicyActive: map[types.NamespacedName]int{polA: 1}},
			queue:       []controller.AdmissionCandidate{candidate(targetRun, polA, 1)},
			target:      targetRun,
			wantMessage: []string{"policy cap 1 (AgentPolicy policy-a) reached"},
		},
		{
			name:      "full policy head does not block another policy",
			state:     controller.AdmissionState{GlobalActive: 1, PolicyActive: map[types.NamespacedName]int{polA: 1}},
			queue:     []controller.AdmissionCandidate{candidate("a-head", polA, 1), candidate("b-later", polB, 1)},
			target:    "b-later",
			wantAdmit: true,
		},
		{
			name:        "earlier run under the same policy takes its last place",
			state:       controller.AdmissionState{PolicyActive: map[types.NamespacedName]int{polA: 1}},
			queue:       []controller.AdmissionCandidate{candidate("earlier", polA, 2), candidate("later", polA, 2)},
			target:      "later",
			wantMessage: []string{"policy cap 2 (AgentPolicy policy-a) reached", "waiting for earlier queued runs"},
		},
		{
			name:      "unresolved earlier run is skipped",
			state:     controller.AdmissionState{GlobalCap: 1},
			queue:     []controller.AdmissionCandidate{{Run: key("broken")}, candidate(targetRun, polA, 0)},
			target:    targetRun,
			wantAdmit: true,
		},
		{
			name:        "target missing from the queue waits",
			state:       controller.AdmissionState{},
			queue:       []controller.AdmissionCandidate{candidate("other", polA, 0)},
			target:      targetRun,
			wantMessage: []string{"not yet in the admission queue"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := controller.SimulateAdmission(tt.state, tt.queue, key(tt.target))
			if got.Admit != tt.wantAdmit {
				t.Fatalf("SimulateAdmission().Admit = %v (%q), want %v", got.Admit, got.Message, tt.wantAdmit)
			}
			for _, fragment := range tt.wantMessage {
				if !strings.Contains(got.Message, fragment) {
					t.Errorf("SimulateAdmission().Message = %q, want it to contain %q", got.Message, fragment)
				}
			}
		})
	}
}
