package manifests_test

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

func TestChatGPTGatewayUsesSingleRefreshOwnerAndScopedCredentialAccess(t *testing.T) {
	docs := renderInstall(t)
	foundDeployment, foundRole := false, false
	for _, doc := range objectsOfKind(docs, "Deployment") {
		if metadataName(doc) != sharedGatewayName {
			continue
		}
		foundDeployment = true
		data, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		var deployment appsv1.Deployment
		if err := json.Unmarshal(data, &deployment); err != nil {
			t.Fatal(err)
		}
		if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 ||
			deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
			t.Fatal("rotating OAuth session requires one non-overlapping refresh owner")
		}
		foundMode := false
		for _, env := range deployment.Spec.Template.Spec.Containers[0].Env {
			if env.Name == "OPENAI_API_KEY" && (env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil ||
				env.ValueFrom.SecretKeyRef.Optional == nil || !*env.ValueFrom.SecretKeyRef.Optional) {
				t.Fatal("ChatGPT mode still requires an API key Secret")
			}
			if env.Name == "MODEL_AUTH_MODE" && env.ValueFrom != nil && env.ValueFrom.ConfigMapKeyRef != nil {
				foundMode = true
			}
		}
		if !foundMode {
			t.Fatal("administrator cannot select model authentication")
		}
	}
	for _, doc := range objectsOfKind(docs, "Role") {
		if metadataName(doc) != "sproozi-shared-gateway-credentials" {
			continue
		}
		foundRole = true
		data, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		var role rbacv1.Role
		if err := json.Unmarshal(data, &role); err != nil {
			t.Fatal(err)
		}
		if role.Namespace != systemNamespace || len(role.Rules) != 1 {
			t.Fatal("credential role is not namespace-scoped")
		}
		rule := role.Rules[0]
		if strings.Join(rule.Resources, ",") != "secrets" ||
			strings.Join(rule.ResourceNames, ",") != "model-gateway-chatgpt" ||
			strings.Join(rule.Verbs, ",") != "get,update" {
			t.Fatal("credential RBAC is broader than the one renewable session")
		}
	}
	if !foundDeployment || !foundRole {
		t.Fatal("gateway deployment or scoped credential RBAC missing")
	}
}

func TestDemoAuthModeSelectsExplicitBudgets(t *testing.T) {
	root := repoRoot(t)
	for _, mode := range []string{"api_key", "chatgpt"} {
		cmd := exec.Command("python3", filepath.Join(root, "hack/demo/render.py"), "--model-auth", mode,
			"--codex-image", "localhost:5001/fixture@sha256:"+strings.Repeat("a", 64), "--repository", "fixture/demo")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("demo render: %v: %s", err, out)
		}
		wantCost := "maxCostMicros: 500000"
		wantTokens := "maxUnits: 100000"
		if mode == "chatgpt" {
			wantCost = "maxCostMicros: 0"
			wantTokens = "maxUnits: 300000"
		}
		if !strings.Contains(string(out), wantCost) || !strings.Contains(string(out), wantTokens) ||
			!strings.Contains(string(out), "--model gpt-6.1-sol") {
			t.Fatal("auth mode did not select the expected budgets and requested model")
		}
		if strings.Contains(string(out), "model-gateway-chatgpt") || strings.Contains(string(out), "refresh_token") {
			t.Fatal("provider credentials leaked into the agent demo bundle")
		}
	}
}
