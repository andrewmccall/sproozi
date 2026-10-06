package manifests_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestSREDemoUsesInspectableResponsesHTTPTransport(t *testing.T) {
	docs := renderKustomization(t, filepath.Join(repoRoot(t), "examples", "sre-demo"))
	runtimes := objectsOfKind(docs, "AgentRuntime")
	if len(runtimes) != 1 {
		t.Fatal("expected one runtime")
	}
	data, err := json.Marshal(runtimes[0])
	if err != nil {
		t.Fatal(err)
	}
	var runtime struct {
		Spec struct {
			PodTemplate corev1.PodTemplateSpec `json:"podTemplate"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(data, &runtime); err != nil {
		t.Fatal(err)
	}
	command := strings.Join(runtime.Spec.PodTemplate.Spec.Containers[0].Args, " ")
	for _, required := range []string{
		"exec codex exec", "--model gpt-6.1-sol", "supports_websockets=false", "wire_api=\"responses\"",
	} {
		if !strings.Contains(command, required) {
			t.Fatalf("stock Codex demo is missing %q", required)
		}
	}
}

// The gateway requires a profile file even when no generic egress is granted.
// Its mandatory mount must resolve within the base installation before demo
// workloads are applied; otherwise the real gateway never starts.
func TestInstalledGatewayHasFailClosedProfileStore(t *testing.T) {
	docs := renderInstall(t)
	var profileName string
	for _, doc := range objectsOfKind(docs, "Deployment") {
		if metadataName(doc) != sharedGatewayName {
			continue
		}
		data, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		var deployment appsv1.Deployment
		if err := json.Unmarshal(data, &deployment); err != nil {
			t.Fatal(err)
		}
		for _, volume := range deployment.Spec.Template.Spec.Volumes {
			if volume.Name == "profiles" && volume.ConfigMap != nil {
				profileName = volume.ConfigMap.Name
			}
		}
	}
	if profileName == "" {
		t.Fatal("gateway has no administrator-owned profile mount")
	}
	for _, doc := range objectsOfKind(docs, "ConfigMap") {
		if metadataName(doc) != profileName || metadataNamespace(doc) != systemNamespace {
			continue
		}
		data, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		var profiles corev1.ConfigMap
		if err := json.Unmarshal(data, &profiles); err != nil {
			t.Fatal(err)
		}
		var store map[string]json.RawMessage
		if err := json.Unmarshal([]byte(profiles.Data["profiles.json"]), &store); err != nil {
			t.Fatalf("gateway profile store is not valid JSON: %v", err)
		}
		if store == nil || len(store) != 0 {
			t.Fatal("base installation must provide an empty, fail-closed profile store")
		}
		return
	}
	t.Fatalf("gateway requires missing ConfigMap %q before demo workloads are applied", profileName)
}

func TestSREDemoExplicitlyDisablesRequiredEgressProfiles(t *testing.T) {
	docs := renderKustomization(t, filepath.Join(repoRoot(t), "examples", "sre-demo"))
	for _, kind := range []string{"AgentPolicy", "AgentTemplate"} {
		objects := objectsOfKind(docs, kind)
		if len(objects) != 1 {
			t.Fatalf("demo must contain one %s", kind)
		}
		spec, ok := objects[0]["spec"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no spec", kind)
		}
		profiles, ok := spec["egressProfiles"].([]any)
		if !ok || len(profiles) != 0 {
			t.Fatalf("%s must explicitly supply the required empty egressProfiles array", kind)
		}
	}
}
