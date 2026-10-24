package kubernetes

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	managedFixtureTemplate    = "approved"
	fixtureRunUID             = "run-uid"
	fixtureNativeInstructions = "Approved native instructions"
	fixtureOperator           = "operator"
	fixtureNativeUUID         = "12345678-1234-1234-1234-123456789abc"
	fixtureRegistrationKey    = "registration"
	fixturePhaseKey           = "phase"
	fixtureRegistryUID        = "registry-uid"
	fixtureRegistrationFile   = "registration.json"
	fixtureGateway            = "https://gateway.invalid:443"
	fixtureRunNamespace       = "runs"
	fixtureSessionID          = "session-1"
	fixtureRunUIDKey          = "runUid"
	fixturePodUIDKey          = "podUid"
	fixtureRegistryLabel      = "sessions.sproozi.com/registry"
	fixtureVersionKey         = "version"
)

func TestManagedLaunchProjectsFreshKindSpecificPlaceholderAndHarness(t *testing.T) {
	for _, kind := range []harness.NativeKind{harness.NativeCodex, harness.NativeClaude} {
		t.Run(string(kind), func(t *testing.T) {
			run := &api.AgentRun{ObjectMeta: metav1.ObjectMeta{Namespace: fixtureRunNamespace, Name: "native-run", UID: fixtureRunUID},
				Spec: api.AgentRunSpec{TemplateRef: api.AgentTemplateReference{Name: managedFixtureTemplate},
					Capabilities: []api.CapabilityKind{api.CapabilityModelInference}}}
			template := &api.AgentTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: run.Namespace, Name: managedFixtureTemplate,
				Annotations: map[string]string{registry.ManagedTemplateAnnotation: managedTemplateEnabledValue}},
				Spec: api.AgentTemplateSpec{Instructions: fixtureNativeInstructions}}
			intent := registry.Registration{Harness: kind, Principal: fixtureOperator, SessionID: fixtureSessionID, Epoch: 1,
				OperationID: "move-1", NativeUUID: fixtureNativeUUID, InstructionDigest: registry.InstructionsDigest(template.Spec.Instructions), OriginalDeadline: time.Now().UTC().Add(time.Hour),
				TemplateName: template.Name, Ceilings: map[api.CapabilityKind]api.EndpointBudget{api.CapabilityModelInference: {MaxUnits: 20}},
				CheckpointDigest: strings.Repeat("a", 64), IntendedRunName: run.Name}
			data, err := json.Marshal(map[string]any{fixtureVersionKey: 1, fixtureRegistrationKey: intent, fixtureRunUIDKey: string(run.UID),
				fixturePodUIDKey: "", fixturePhaseKey: registry.Bound})
			if err != nil {
				t.Fatal(err)
			}
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: run.Namespace, Name: registry.NameForRun(run.Name),
				UID: fixtureRegistryUID, Labels: map[string]string{fixtureRegistryLabel: managedTemplateEnabledValue}},
				Data: map[string]string{fixtureRegistrationFile: string(data)}}
			scheme := runtime.NewScheme()
			if err = corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
			rt := &api.AgentRuntime{Spec: api.AgentRuntimeSpec{GatewayEndpoint: fixtureGateway,
				ClientConfig: api.RuntimeClientConfig{Harness: string(kind), NativeSession: &api.NativeSessionConfig{Model: "approved-model"}}}}
			contract := map[string]string{}
			if err = addManagedNativeContract(t.Context(), kube, run, template, rt, &corev1.ConfigMap{Data: contract}); err != nil {
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
				!launch.Deadline.Equal(intent.OriginalDeadline) || !slices.Contains(launch.Env, key+"=sproozi-worker-placeholder") {
				t.Fatalf("wrong trusted native launch: %+v", launch)
			}
			for _, entry := range launch.Env {
				if strings.HasPrefix(entry, other+"=") {
					t.Fatal("launch inherited another provider's credential")
				}
			}
			// A runtime with the harness's own sign-in gets no placeholder, and
			// the supervisor restores the login from the sign-in directory.
			rt.Spec.ClientConfig.SignIn = &api.RuntimeSignIn{Claims: &api.RuntimeLoginClaims{}}
			signedIn := map[string]string{}
			if err = addManagedNativeContract(t.Context(), kube, run, template, rt, &corev1.ConfigMap{Data: signedIn}); err != nil {
				t.Fatal(err)
			}
			launch = harness.NativeLaunch{}
			if err = json.Unmarshal([]byte(signedIn[nativeConfigKey]), &launch); err != nil {
				t.Fatal(err)
			}
			if launch.SignInDirectory != api.SignInDirectory || slices.Contains(launch.Env, key+"=sproozi-worker-placeholder") {
				t.Fatalf("signed-in launch: directory=%q env=%v", launch.SignInDirectory, launch.Env)
			}
			rt.Spec.ClientConfig.SignIn = nil
			rt.Spec.ClientConfig.Harness = "mismatched"
			if err = addManagedNativeContract(t.Context(), kube, run, template, rt, &corev1.ConfigMap{Data: map[string]string{}}); err == nil {
				t.Fatal("launch accepted client/registration mismatch")
			}
		})
	}
}

func TestInitialNativeContractRetainsTaskAndEmptyThreadAfterBinding(t *testing.T) {
	run := &api.AgentRun{ObjectMeta: metav1.ObjectMeta{Namespace: fixtureRunNamespace, Name: "initial-run", UID: fixtureRunUID}, Spec: api.AgentRunSpec{TemplateRef: api.AgentTemplateReference{Name: managedFixtureTemplate}, Capabilities: []api.CapabilityKind{api.CapabilityModelInference}}}
	template := &api.AgentTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: run.Namespace, Name: managedFixtureTemplate, Annotations: map[string]string{registry.ManagedTemplateAnnotation: managedTemplateEnabledValue}}, Spec: api.AgentTemplateSpec{Instructions: fixtureNativeInstructions}}
	intent := registry.Registration{Harness: harness.NativeCodex, Principal: fixtureOperator, SessionID: fixtureSessionID, Epoch: 1, OperationID: "initial-1", InstructionDigest: registry.InstructionsDigest(template.Spec.Instructions), OriginalDeadline: time.Now().UTC().Add(time.Hour), TemplateName: template.Name, Ceilings: map[api.CapabilityKind]api.EndpointBudget{api.CapabilityModelInference: {MaxUnits: 20}}, WorkspaceDigest: strings.Repeat("a", 64), Task: "Fix the fixture", IntendedRunName: run.Name}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	document := func() string {
		data, err := json.Marshal(map[string]any{fixtureVersionKey: 1, fixtureRegistrationKey: intent, fixtureRunUIDKey: string(run.UID), fixturePodUIDKey: "pod-uid", fixturePhaseKey: registry.Active})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: run.Namespace, Name: registry.NameForRun(run.Name), UID: fixtureRegistryUID, Labels: map[string]string{fixtureRegistryLabel: managedTemplateEnabledValue}}, Data: map[string]string{fixtureRegistrationFile: document()}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
	rt := &api.AgentRuntime{Spec: api.AgentRuntimeSpec{GatewayEndpoint: fixtureGateway, ClientConfig: api.RuntimeClientConfig{Harness: string(harness.NativeCodex)}}}
	before := map[string]string{}
	if err := addManagedNativeContract(t.Context(), kube, run, template, rt, &corev1.ConfigMap{Data: before}); err != nil {
		t.Fatal(err)
	}
	var launch harness.NativeLaunch
	if err := json.Unmarshal([]byte(before[nativeConfigKey]), &launch); err != nil {
		t.Fatal(err)
	}
	if launch.ThreadID != "" || launch.Task != "Fix the fixture" {
		t.Fatalf("initial task/identity changed: %+v", launch)
	}
	intent.NativeUUID = fixtureNativeUUID
	cm.Data[fixtureRegistrationFile] = document()
	if err := kube.Update(t.Context(), cm); err != nil {
		t.Fatal(err)
	}
	after := map[string]string{}
	if err := addManagedNativeContract(t.Context(), kube, run, template, rt, &corev1.ConfigMap{Data: after}); err != nil {
		t.Fatal(err)
	}
	if before[nativeConfigKey] != after[nativeConfigKey] {
		t.Fatal("native UUID binding rewrote immutable initial launch configuration")
	}
}

// The controller copies the stored profile into the run's immutable contract
// after checking its digest, and fails rather than launch without it.
func TestManagedContractCarriesTheRegisteredProfile(t *testing.T) {
	run := &api.AgentRun{ObjectMeta: metav1.ObjectMeta{Namespace: fixtureRunNamespace, Name: "profile-run", UID: fixtureRunUID},
		Spec: api.AgentRunSpec{TemplateRef: api.AgentTemplateReference{Name: managedFixtureTemplate},
			Capabilities: []api.CapabilityKind{api.CapabilityModelInference}}}
	template := &api.AgentTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: run.Namespace, Name: managedFixtureTemplate,
		Annotations: map[string]string{registry.ManagedTemplateAnnotation: managedTemplateEnabledValue}},
		Spec: api.AgentTemplateSpec{Instructions: fixtureNativeInstructions}}
	archive, digest, err := agentprofile.Profile{Instructions: "Profile instructions"}.Archive()
	if err != nil {
		t.Fatal(err)
	}
	intent := registry.Registration{Harness: harness.NativeCodex, Principal: fixtureOperator, SessionID: fixtureSessionID, Epoch: 1,
		OperationID: "move-1", NativeUUID: fixtureNativeUUID,
		InstructionDigest: registry.InstructionsDigest(template.Spec.Instructions), OriginalDeadline: time.Now().UTC().Add(time.Hour),
		TemplateName: template.Name, Ceilings: map[api.CapabilityKind]api.EndpointBudget{api.CapabilityModelInference: {MaxUnits: 20}},
		CheckpointDigest: strings.Repeat("a", 64), IntendedRunName: run.Name, ProfileDigest: digest}
	data, err := json.Marshal(map[string]any{fixtureVersionKey: 1, fixtureRegistrationKey: intent, fixtureRunUIDKey: string(run.UID),
		fixturePodUIDKey: "", fixturePhaseKey: registry.Bound})
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: run.Namespace, Name: registry.NameForRun(run.Name),
		UID: fixtureRegistryUID, Labels: map[string]string{fixtureRegistryLabel: managedTemplateEnabledValue}},
		Data: map[string]string{fixtureRegistrationFile: string(data)}}
	scheme := runtime.NewScheme()
	if err = corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
	rt := &api.AgentRuntime{Spec: api.AgentRuntimeSpec{GatewayEndpoint: fixtureGateway,
		ClientConfig: api.RuntimeClientConfig{Harness: string(harness.NativeCodex)}}}
	if err = addManagedNativeContract(t.Context(), kube, run, template, rt, &corev1.ConfigMap{Data: map[string]string{}}); err == nil {
		t.Fatal("contract built without the registered profile")
	}
	if err = registry.StoreProfile(t.Context(), kube, run.Namespace, digest, archive); err != nil {
		t.Fatal(err)
	}
	contract := &corev1.ConfigMap{Data: map[string]string{}}
	if err = addManagedNativeContract(t.Context(), kube, run, template, rt, contract); err != nil {
		t.Fatal(err)
	}
	if string(contract.BinaryData[registry.ProfileArchiveKey]) != string(archive) || len(contract.BinaryData) != 1 {
		t.Fatalf("contract profile = %d bytes", len(contract.BinaryData[registry.ProfileArchiveKey]))
	}
}
