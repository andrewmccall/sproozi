package manifests_test

import "testing"

func TestTrustedDeploymentsStayRestricted(t *testing.T) {
	deployments := 0
	for _, doc := range renderDefault(t) {
		if doc["kind"] != "Deployment" {
			continue
		}
		deployments++
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
	if deployments == 0 {
		t.Fatal("expected config/default to render Deployments")
	}
}
