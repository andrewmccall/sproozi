package manifests_test

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

const (
	agentsNamespace = "sproozi-agents"
	verbGet         = "get"
)

func TestAgentRBACBootstrapsNamespaceAndBindsController(t *testing.T) {
	docs := renderKustomization(t, filepath.Join(repoRoot(t), "config", "agent-rbac"))
	var namespace, controllerBinding bool
	for _, doc := range docs {
		if doc["kind"] == "Namespace" && metadataName(doc) == agentsNamespace {
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

func decodeRules(t *testing.T, doc map[string]any) []rbacv1.PolicyRule {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var role rbacv1.ClusterRole
	if err := json.Unmarshal(data, &role); err != nil {
		t.Fatal(err)
	}
	return role.Rules
}

func grants(rules []rbacv1.PolicyRule, group, resource, verb string) bool {
	for _, rule := range rules {
		if slices.Contains(rule.APIGroups, group) && slices.Contains(rule.Resources, resource) &&
			slices.Contains(rule.Verbs, verb) {
			return true
		}
	}
	return false
}

// installedControllerClusterRules returns the rules of the ClusterRole bound
// to the controller across the cluster.
func installedControllerClusterRules(t *testing.T) []rbacv1.PolicyRule {
	t.Helper()
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
		t.Fatal("installed controller has no cluster role binding")
	}
	for _, doc := range objectsOfKind(docs, "ClusterRole") {
		if metadataName(doc) == roleName {
			return decodeRules(t, doc)
		}
	}
	t.Fatalf("bound controller role %q is absent", roleName)
	return nil
}

// sandboxWriterRules returns the rules of the Role that config/agent-rbac
// binds to the controller in sproozi-agents.
func sandboxWriterRules(t *testing.T) []rbacv1.PolicyRule {
	t.Helper()
	docs := renderKustomization(t, filepath.Join(repoRoot(t), "config", "agent-rbac"))
	var roleName string
	for _, doc := range objectsOfKind(docs, "RoleBinding") {
		if metadataNamespace(doc) != agentsNamespace {
			continue
		}
		subjects, _ := doc["subjects"].([]any)
		for _, raw := range subjects {
			subject, _ := raw.(map[string]any)
			if subject["kind"] == "ServiceAccount" && subject["name"] == controllerManagerName &&
				subject["namespace"] == systemNamespace {
				ref, _ := doc["roleRef"].(map[string]any)
				roleName, _ = ref["name"].(string)
			}
		}
	}
	for _, doc := range objectsOfKind(docs, "Role") {
		if metadataName(doc) == roleName && metadataNamespace(doc) == agentsNamespace {
			return decodeRules(t, doc)
		}
	}
	t.Fatalf("controller has no sandbox writer Role in %s", agentsNamespace)
	return nil
}

type apiResource struct{ group, name string }

func TestControllerWritesSandboxObjectsOnlyInAgentsNamespace(t *testing.T) {
	const rbacGroup = "rbac.authorization.k8s.io"
	pods := apiResource{"", "pods"}
	serviceAccounts := apiResource{"", "serviceaccounts"}
	configMaps := apiResource{"", "configmaps"}
	networkPolicies := apiResource{"networking.k8s.io", "networkpolicies"}
	cluster := installedControllerClusterRules(t)
	sandbox := sandboxWriterRules(t)
	for _, want := range []apiResource{pods, serviceAccounts, configMaps, networkPolicies} {
		for _, verb := range []string{"create", "update", "patch", "delete", "deletecollection"} {
			if grants(cluster, want.group, want.name, verb) {
				t.Errorf("controller ClusterRole grants %s %s in every namespace", verb, want.name)
			}
		}
		if !grants(sandbox, want.group, want.name, "create") || !grants(sandbox, want.group, want.name, "delete") {
			t.Errorf("sandbox writer Role cannot create and delete %s", want.name)
		}
	}
	// Existing run NetworkPolicies are reconciled in place.
	if !grants(sandbox, networkPolicies.group, networkPolicies.name, "update") {
		t.Error("sandbox writer Role cannot update an existing run NetworkPolicy")
	}
	// The Pod watch and the cached ServiceAccount and NetworkPolicy reads need
	// cluster-wide informers; managed runs read their registry ConfigMap in
	// the run namespace.
	informer := []string{"list", "watch"}
	reads := map[apiResource][]string{
		pods: informer, serviceAccounts: informer, networkPolicies: informer, configMaps: {verbGet},
	}
	for want, verbs := range reads {
		for _, verb := range verbs {
			if !grants(cluster, want.group, want.name, verb) {
				t.Errorf("controller ClusterRole lacks %s %s", verb, want.name)
			}
		}
	}
	unused := []apiResource{{"", "pods/log"}, {"", "secrets"}, {rbacGroup, "roles"}, {rbacGroup, "rolebindings"}}
	for _, resource := range unused {
		for _, rule := range cluster {
			if slices.Contains(rule.APIGroups, resource.group) && slices.Contains(rule.Resources, resource.name) {
				t.Errorf("controller ClusterRole grants unused access to %s", resource.name)
			}
		}
	}
}
