package envtest

import (
	"context"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

func TestAgentRunRejectsUnknownPhase(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := sprooziv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	testEnvironment := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
	}
	configuration, err := testEnvironment.Start()
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := testEnvironment.Stop(); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	})

	apiClient, err := client.New(configuration, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	run := &sprooziv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "incident", Namespace: testNamespace},
		Spec: sprooziv1alpha1.AgentRunSpec{
			TemplateRef:  sprooziv1alpha1.AgentTemplateReference{Name: testPolicyName},
			Task:         "Investigate the incident.",
			Capabilities: []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityKubernetesRead, testMCPCapability},
		},
	}
	if err := apiClient.Create(context.Background(), run); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	run.Status.Phase = "Unknown"
	if err := apiClient.Status().Update(context.Background(), run); err == nil {
		t.Fatal("Status().Update() error = nil, want API server validation error")
	}
}
