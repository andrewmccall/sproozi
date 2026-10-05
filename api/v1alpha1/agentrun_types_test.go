package v1alpha1

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAgentRunPhaseConstants(t *testing.T) {
	t.Parallel()

	want := []AgentRunPhase{
		AgentRunPhaseQueued,
		AgentRunPhaseAdmitted,
		AgentRunPhaseRunning,
		AgentRunPhaseSucceeded,
		AgentRunPhaseFailed,
		AgentRunPhaseTimedOut,
		AgentRunPhaseCancelled,
	}

	for _, phase := range want {
		if phase == "" {
			t.Fatal("AgentRun phase constants must have non-empty wire values")
		}
	}
}

func TestAgentRunJSONRoundTrip(t *testing.T) {
	t.Parallel()

	want := AgentRun{
		Spec: AgentRunSpec{
			TemplateRef: AgentTemplateReference{Name: "sre-remediation"},
			Task:        "Investigate the CrashLoopBackOff incident.",
			EventContext: map[string]string{
				"alertName": "SprooziDemoCrashLoopBackOff",
				"namespace": "sproozi-demo",
			},
			Capabilities: []CapabilityKind{
				CapabilityKubernetesRead,
				CapabilityGitHubPullRequest,
			},
			Cancel: true,
		},
		Status: AgentRunStatus{
			Phase: AgentRunPhaseRunning,
			Identity: AgentRunIdentity{
				ServiceAccountName: "agentrun-123",
				SandboxName:        "agentrun-123",
			},
			StartedAt:   &metav1.Time{Time: time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)},
			CompletedAt: &metav1.Time{Time: time.Date(2026, time.August, 29, 13, 0, 0, 0, time.UTC)},
		},
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got AgentRun
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if !reflect.DeepEqual(got.Spec, want.Spec) {
		t.Fatalf("round-trip spec = %#v, want %#v", got.Spec, want.Spec)
	}

	if got.Status.Phase != want.Status.Phase || !reflect.DeepEqual(got.Status.Identity, want.Status.Identity) {
		t.Fatalf("round-trip status = %#v, want %#v", got.Status, want.Status)
	}

	if got.Status.StartedAt == nil || !got.Status.StartedAt.Equal(want.Status.StartedAt) {
		t.Fatalf("round-trip startedAt = %v, want %v", got.Status.StartedAt, want.Status.StartedAt)
	}

	if got.Status.CompletedAt == nil || !got.Status.CompletedAt.Equal(want.Status.CompletedAt) {
		t.Fatalf("round-trip completedAt = %v, want %v", got.Status.CompletedAt, want.Status.CompletedAt)
	}
}
