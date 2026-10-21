package manifests_test

import (
	"path/filepath"
	"testing"
)

// Examples are applied client-side after config/agent-rbac. A redeclared
// sandbox namespace without the installed labels would remove them, including
// Pod Security restricted enforcement.
func TestExamplesKeepSandboxNamespaceLabels(t *testing.T) {
	root := repoRoot(t)
	want := namespaceLabels(renderKustomization(t, filepath.Join(root, "config", "agent-rbac")), agentsNamespace)
	if want["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Fatalf("installed sandbox namespace does not enforce restricted Pod Security: %v", want)
	}
	examples, err := filepath.Glob(filepath.Join(root, "examples", "*", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	overlays, err := filepath.Glob(filepath.Join(root, "examples", "*", "*", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, kustomization := range append(examples, overlays...) {
		dir := filepath.Dir(kustomization)
		got := namespaceLabels(renderKustomization(t, dir), agentsNamespace)
		if got == nil {
			continue
		}
		for key, value := range want {
			if got[key] != value {
				t.Errorf("%s redeclares sproozi-agents without label %s=%s", dir, key, value)
			}
		}
	}
}

// namespaceLabels returns the labels of the named Namespace, or nil when the
// documents do not declare it.
func namespaceLabels(docs []map[string]any, name string) map[string]string {
	for _, doc := range objectsOfKind(docs, "Namespace") {
		if metadataName(doc) != name {
			continue
		}
		labels := map[string]string{}
		metadata, _ := doc["metadata"].(map[string]any)
		raw, _ := metadata["labels"].(map[string]any)
		for key, value := range raw {
			labels[key], _ = value.(string)
		}
		return labels
	}
	return nil
}
