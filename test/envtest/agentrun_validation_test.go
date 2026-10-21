package envtest

import (
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

func TestAgentRunRejectsUnknownPhase(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	run := &sprooziv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "incident", Namespace: newNamespace(t)},
		Spec: sprooziv1alpha1.AgentRunSpec{
			TemplateRef:  sprooziv1alpha1.AgentTemplateReference{Name: testPolicyName},
			Task:         "Investigate the incident.",
			Capabilities: []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityKubernetesRead, testMCPCapability},
		},
	}
	if err := testClient.Create(ctx, run); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// The shared controller may write status concurrently; only a validation
	// error proves the API server rejected the phase.
	for {
		if err := testClient.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		run.Status.Phase = "Unknown"
		err := testClient.Status().Update(ctx, run)
		if apierrors.IsConflict(err) {
			continue
		}
		if !apierrors.IsInvalid(err) {
			t.Fatalf("Status().Update() error = %v, want API server validation error", err)
		}
		return
	}
}

// The API server rejects input the agent contract would refuse later, so a bad
// run never reaches admission.
func TestAgentRunRejectsInputTheContractRefuses(t *testing.T) {
	t.Parallel()
	ns := newNamespace(t)

	tests := map[string]func(*sprooziv1alpha1.AgentRun){
		"empty task": func(run *sprooziv1alpha1.AgentRun) { run.Spec.Task = "" },
		"duplicate capability": func(run *sprooziv1alpha1.AgentRun) {
			run.Spec.Capabilities = []sprooziv1alpha1.CapabilityKind{
				sprooziv1alpha1.CapabilityKubernetesRead, sprooziv1alpha1.CapabilityKubernetesRead}
		},
		"empty template name": func(run *sprooziv1alpha1.AgentRun) { run.Spec.TemplateRef.Name = "" },
		"name longer than a label value": func(run *sprooziv1alpha1.AgentRun) {
			run.Name = "invalid-input-" + strings.Repeat("a", 60)
		},
	}
	for name, mutate := range tests {
		run := &sprooziv1alpha1.AgentRun{
			ObjectMeta: metav1.ObjectMeta{Name: "invalid-input", Namespace: ns},
			Spec: sprooziv1alpha1.AgentRunSpec{
				TemplateRef:  sprooziv1alpha1.AgentTemplateReference{Name: testPolicyName},
				Task:         "Investigate the incident.",
				Capabilities: []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityKubernetesRead},
			},
		}
		mutate(run)
		if err := testClient.Create(t.Context(), run); !apierrors.IsInvalid(err) {
			t.Errorf("%s: Create() error = %v, want an Invalid error", name, err)
		}
	}
}
