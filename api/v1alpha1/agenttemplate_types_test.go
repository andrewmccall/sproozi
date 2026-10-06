package v1alpha1

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestAgentTemplateJSONRoundTrip(t *testing.T) {
	t.Parallel()

	want := AgentTemplate{
		Spec: AgentTemplateSpec{
			RuntimeRef:   AgentRuntimeReference{Name: "codex"},
			PolicyRef:    AgentPolicyReference{Name: "sre"},
			Instructions: "Investigate the incident and create a pull request when a safe remediation is found.",
			EgressProfiles: []string{
				"go-modules",
				"npm",
			},
		},
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got AgentTemplate
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if !reflect.DeepEqual(got.Spec, want.Spec) {
		t.Fatalf("round-trip spec = %#v, want %#v", got.Spec, want.Spec)
	}
}
