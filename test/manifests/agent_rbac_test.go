package manifests_test

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

func TestAgentRBACBootstrapsNamespaceAndBindsController(t *testing.T) {
	docs := renderKustomization(t, filepath.Join(repoRoot(t), "config", "agent-rbac"))
	var namespace, controllerBinding bool
	for _, doc := range docs {
		if doc["kind"] == "Namespace" && metadataName(doc) == "sproozi-agents" {
			namespace = true
		}
		if doc["kind"] != "RoleBinding" {
			continue
		}
		subjects, _ := doc["subjects"].([]any)
		for _, raw := range subjects {
			subject, _ := raw.(map[string]any)
			controllerBinding = controllerBinding || (subject["kind"] == "ServiceAccount" &&
				subject["name"] == controllerManagerName && subject["namespace"] == systemNamespace)
		}
	}
	if !namespace || !controllerBinding {
		t.Fatalf("sandbox RBAC cannot bootstrap its namespace or bind the controller: namespace=%t binding=%t",
			namespace, controllerBinding)
	}
}

func TestInstalledControllerCanReconcileExistingNetworkPolicies(t *testing.T) {
	docs := renderInstall(t)
	var roleName string
	for _, doc := range objectsOfKind(docs, "ClusterRoleBinding") {
		if metadataName(doc) != "sproozi-controller-manager-rolebinding" {
			continue
		}
		ref, _ := doc["roleRef"].(map[string]any)
		roleName, _ = ref["name"].(string)
	}
	if roleName == "" {
		t.Fatal("installed controller has no role binding")
	}
	for _, doc := range objectsOfKind(docs, "ClusterRole") {
		if metadataName(doc) != roleName {
			continue
		}
		data, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		var role rbacv1.ClusterRole
		if err := json.Unmarshal(data, &role); err != nil {
			t.Fatal(err)
		}
		for _, rule := range role.Rules {
			if slices.Contains(rule.APIGroups, "networking.k8s.io") &&
				slices.Contains(rule.Resources, "networkpolicies") && slices.Contains(rule.Verbs, "update") {
				return
			}
		}
		t.Fatalf("bound controller role %q cannot update an existing run NetworkPolicy", roleName)
	}
	t.Fatalf("bound controller role %q is absent", roleName)
}
