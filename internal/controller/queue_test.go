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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

func TestActiveRun(t *testing.T) {
	t.Parallel()

	now := time.Now()

	tests := []struct {
		name     string
		runs     []sprooziv1alpha1.AgentRun
		wantName string // "" means nil expected
	}{
		{
			name:     "empty list",
			runs:     nil,
			wantName: "",
		},
		{
			name:     "only queued runs",
			runs:     []sprooziv1alpha1.AgentRun{runWithPhase("a", sprooziv1alpha1.AgentRunPhaseQueued, now)},
			wantName: "",
		},
		{
			name:     "admitted run is active",
			runs:     []sprooziv1alpha1.AgentRun{runWithPhase("a", sprooziv1alpha1.AgentRunPhaseAdmitted, now)},
			wantName: "a",
		},
		{
			name:     "running run is active",
			runs:     []sprooziv1alpha1.AgentRun{runWithPhase("a", sprooziv1alpha1.AgentRunPhaseRunning, now)},
			wantName: "a",
		},
		{
			name: "terminal runs are not active",
			runs: []sprooziv1alpha1.AgentRun{
				runWithPhase("a", sprooziv1alpha1.AgentRunPhaseSucceeded, now),
				runWithPhase("b", sprooziv1alpha1.AgentRunPhaseFailed, now),
				runWithPhase("c", sprooziv1alpha1.AgentRunPhaseCancelled, now),
				runWithPhase("d", sprooziv1alpha1.AgentRunPhaseTimedOut, now),
			},
			wantName: "",
		},
		{
			name: "admitted run returned among mixed phases",
			runs: []sprooziv1alpha1.AgentRun{
				runWithPhase("a", sprooziv1alpha1.AgentRunPhaseQueued, now),
				runWithPhase("b", sprooziv1alpha1.AgentRunPhaseAdmitted, now),
				runWithPhase("c", sprooziv1alpha1.AgentRunPhaseSucceeded, now),
			},
			wantName: "b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := controller.ActiveRun(tt.runs)
			if tt.wantName == "" {
				if got != nil {
					t.Errorf("ActiveRun() = %q, want nil", got.Name)
				}
			} else {
				if got == nil {
					t.Fatalf("ActiveRun() = nil, want %q", tt.wantName)
				}
				if got.Name != tt.wantName {
					t.Errorf("ActiveRun().Name = %q, want %q", got.Name, tt.wantName)
				}
			}
		})
	}
}

func TestNextQueued(t *testing.T) {
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
			got := controller.NextQueued(tt.runs)
			if tt.wantName == "" {
				if got != nil {
					t.Errorf("NextQueued() = %q, want nil", got.Name)
				}
			} else {
				if got == nil {
					t.Fatalf("NextQueued() = nil, want %q", tt.wantName)
				}
				if got.Name != tt.wantName {
					t.Errorf("NextQueued().Name = %q, want %q", got.Name, tt.wantName)
				}
			}
		})
	}
}
