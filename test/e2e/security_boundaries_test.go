package e2e_test

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"k8s.io/apimachinery/pkg/util/yaml"
)

func e2eRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func renderDefaultManifests(t *testing.T) []map[string]any {
	t.Helper()
	root := e2eRepoRoot(t)

	makeCmd := exec.Command("make", "kustomize")
	makeCmd.Dir = root
	if out, err := makeCmd.CombinedOutput(); err != nil {
		t.Fatalf("make kustomize: %v\n%s", err, out)
	}

	buildCmd := exec.Command(filepath.Join(root, "bin", "kustomize"), "build", filepath.Join(root, "config", "default"))
	buildCmd.Dir = root
	out, err := buildCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kustomize build: %v\n%s", err, out)
	}

	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	var docs []map[string]any
	for {
		var doc map[string]any
		if err := decoder.Decode(&doc); err != nil {
			break
		}
		if len(doc) == 0 {
			continue
		}
		docs = append(docs, doc)
	}
	return docs
}

func TestTrustedDeploymentsStayRestricted(t *testing.T) {
	docs := renderDefaultManifests(t)
	for _, doc := range docs {
		if doc["kind"] != "Deployment" {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		podSpec, _ := template["spec"].(map[string]any)
		containers, _ := podSpec["containers"].([]any)
		for _, raw := range containers {
			container, _ := raw.(map[string]any)
			securityContext, _ := container["securityContext"].(map[string]any)
			if securityContext["allowPrivilegeEscalation"] != false {
				t.Fatalf("expected allowPrivilegeEscalation=false for deployment %q", metadataName(doc))
			}
			if securityContext["readOnlyRootFilesystem"] != true {
				t.Fatalf("expected readOnlyRootFilesystem=true for deployment %q", metadataName(doc))
			}
			if privileged, ok := securityContext["privileged"].(bool); ok && privileged {
				t.Fatalf("expected privileged=false for deployment %q", metadataName(doc))
			}
		}
	}
}

func metadataName(doc map[string]any) string {
	metadata, _ := doc["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	return name
}
