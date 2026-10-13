package kubernetes

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/harness"
	registry "github.com/andrewmccall/sproozi/internal/runtime/kubernetes/session"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	managedFixtureTemplate = "approved"
)

func TestManagedLaunchProjectsFreshKindSpecificPlaceholderAndHarness(t *testing.T) {
	for _, kind := range []harness.NativeKind{harness.NativeCodex, harness.NativeClaude} {
		t.Run(string(kind), func(t *testing.T) {
			version, err := harness.NativeVersion(kind)
			if err != nil {
				t.Fatal(err)
			}
			run := &api.AgentRun{ObjectMeta: metav1.ObjectMeta{Namespace: "runs", Name: "native-run", UID: "run-uid"},
				Spec: api.AgentRunSpec{TemplateRef: api.AgentTemplateReference{Name: managedFixtureTemplate},
					Capabilities: []api.CapabilityKind{api.CapabilityModelInference}}}
			template := &api.AgentTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: run.Namespace, Name: managedFixtureTemplate,
				Annotations: map[string]string{registry.ManagedTemplateAnnotation: managedTemplateEnabledValue}},
				Spec: api.AgentTemplateSpec{Instructions: "Approved native instructions"}}
			intent := registry.Registration{Harness: kind, Principal: "operator", SessionID: "session-1", Epoch: 1,
				OperationID: "move-1", NativeUUID: "12345678-1234-1234-1234-123456789abc", NativePin: version,
				InstructionDigest: registry.InstructionsDigest(template.Spec.Instructions), OriginalDeadline: time.Now().UTC().Add(time.Hour),
				TemplateName: template.Name, Ceilings: map[api.CapabilityKind]api.EndpointBudget{api.CapabilityModelInference: {MaxUnits: 20}},
				CheckpointDigest: strings.Repeat("a", 64), IntendedRunName: run.Name}
			data, err := json.Marshal(map[string]any{"version": 1, "registration": intent, "runUid": string(run.UID),
				"podUid": "", "phase": registry.Bound})
			if err != nil {
				t.Fatal(err)
			}
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: run.Namespace, Name: registry.NameForRun(run.Name),
				UID: "registry-uid", Labels: map[string]string{"sessions.sproozi.com/registry": managedTemplateEnabledValue}},
				Data: map[string]string{"registration.json": string(data)}}
			scheme := runtime.NewScheme()
			if err = corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
			rt := &api.AgentRuntime{Spec: api.AgentRuntimeSpec{GatewayEndpoint: "https://gateway.invalid:443",
				ClientConfig: api.RuntimeClientConfig{Harness: string(kind), NativeSession: &api.NativeSessionConfig{Model: "approved-model"}}}}
			contract := map[string]string{}
			if err = addManagedNativeContract(t.Context(), kube, run, template, rt, contract); err != nil {
				t.Fatal(err)
			}
			var launch harness.NativeLaunch
			if err = json.Unmarshal([]byte(contract[nativeConfigKey]), &launch); err != nil {
				t.Fatal(err)
			}
			key, other := openAIAPIKey, "ANTHROPIC_API_KEY"
			if kind == harness.NativeClaude {
				key, other = other, key
			}
			if launch.Harness != kind || launch.Model != "approved-model" || launch.ThreadID != intent.NativeUUID ||
				!launch.Deadline.Equal(intent.OriginalDeadline) || !slices.Contains(launch.Env, key+"="+clientAuthPlaceholder) {
				t.Fatalf("wrong trusted native launch: %+v", launch)
			}
			for _, entry := range launch.Env {
				if strings.HasPrefix(entry, other+"=") {
					t.Fatal("launch inherited another provider's credential")
				}
			}
			rt.Spec.ClientConfig.Harness = "mismatched"
			if err = addManagedNativeContract(t.Context(), kube, run, template, rt, map[string]string{}); err == nil {
				t.Fatal("launch accepted client/registration mismatch")
			}
		})
	}
}

func TestInitialNativeContractRetainsTaskAndEmptyThreadAfterBinding(t *testing.T) {
	run := &api.AgentRun{ObjectMeta: metav1.ObjectMeta{Namespace: "runs", Name: "initial-run", UID: "run-uid"}, Spec: api.AgentRunSpec{TemplateRef: api.AgentTemplateReference{Name: managedFixtureTemplate}, Capabilities: []api.CapabilityKind{api.CapabilityModelInference}}}
	template := &api.AgentTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: run.Namespace, Name: managedFixtureTemplate, Annotations: map[string]string{registry.ManagedTemplateAnnotation: managedTemplateEnabledValue}}, Spec: api.AgentTemplateSpec{Instructions: "Approved native instructions"}}
	intent := registry.Registration{Harness: harness.NativeCodex, Principal: "operator", SessionID: "session-1", Epoch: 1, OperationID: "initial-1", NativePin: harness.CodexSessionVersion, InstructionDigest: registry.InstructionsDigest(template.Spec.Instructions), OriginalDeadline: time.Now().UTC().Add(time.Hour), TemplateName: template.Name, Ceilings: map[api.CapabilityKind]api.EndpointBudget{api.CapabilityModelInference: {MaxUnits: 20}}, WorkspaceDigest: strings.Repeat("a", 64), Task: "Fix the fixture", IntendedRunName: run.Name}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	document := func() string {
		data, err := json.Marshal(map[string]any{"version": 1, "registration": intent, "runUid": string(run.UID), "podUid": "pod-uid", "phase": registry.Active})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: run.Namespace, Name: registry.NameForRun(run.Name), UID: "registry-uid", Labels: map[string]string{"sessions.sproozi.com/registry": managedTemplateEnabledValue}}, Data: map[string]string{"registration.json": document()}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
	rt := &api.AgentRuntime{Spec: api.AgentRuntimeSpec{GatewayEndpoint: "https://gateway.invalid:443", ClientConfig: api.RuntimeClientConfig{Harness: string(harness.NativeCodex)}}}
	before := map[string]string{}
	if err := addManagedNativeContract(t.Context(), kube, run, template, rt, before); err != nil {
		t.Fatal(err)
	}
	var launch harness.NativeLaunch
	if err := json.Unmarshal([]byte(before[nativeConfigKey]), &launch); err != nil {
		t.Fatal(err)
	}
	if launch.ThreadID != "" || launch.Task != "Fix the fixture" {
		t.Fatalf("initial task/identity changed: %+v", launch)
	}
	intent.NativeUUID = "12345678-1234-1234-1234-123456789abc"
	cm.Data["registration.json"] = document()
	if err := kube.Update(t.Context(), cm); err != nil {
		t.Fatal(err)
	}
	after := map[string]string{}
	if err := addManagedNativeContract(t.Context(), kube, run, template, rt, after); err != nil {
		t.Fatal(err)
	}
	if before[nativeConfigKey] != after[nativeConfigKey] {
		t.Fatal("native UUID binding rewrote immutable initial launch configuration")
	}
}
