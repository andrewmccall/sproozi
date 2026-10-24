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
		if doc[keyKind] == "Namespace" && metadataName(doc) == agentsNamespace {
			namespace = true
		}
		if doc[keyKind] != kindRoleBinding {
			continue
		}
		subjects, _ := doc["subjects"].([]any)
		for _, raw := range subjects {
			subject, _ := raw.(map[string]any)
			controllerBinding = controllerBinding || (subject[keyKind] == rbacv1.ServiceAccountKind &&
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
	for _, doc := range objectsOfKind(docs, kindClusterRole) {
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
	for _, doc := range objectsOfKind(docs, kindRoleBinding) {
		if metadataNamespace(doc) != agentsNamespace {
			continue
		}
		subjects, _ := doc["subjects"].([]any)
		for _, raw := range subjects {
			subject, _ := raw.(map[string]any)
			if subject[keyKind] == rbacv1.ServiceAccountKind && subject["name"] == controllerManagerName &&
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
	pods := apiResource{"", resourcePods}
	serviceAccounts := apiResource{"", "serviceaccounts"}
	configMaps := apiResource{"", resourceConfigMaps}
	networkPolicies := apiResource{"networking.k8s.io", "networkpolicies"}
	cluster := installedControllerClusterRules(t)
	sandbox := sandboxWriterRules(t)
	for _, want := range []apiResource{pods, serviceAccounts, configMaps, networkPolicies} {
		for _, verb := range []string{verbCreate, verbUpdate, verbPatch, verbDelete, "deletecollection"} {
			if grants(cluster, want.group, want.name, verb) {
				t.Errorf("controller ClusterRole grants %s %s in every namespace", verb, want.name)
			}
		}
		if !grants(sandbox, want.group, want.name, verbCreate) || !grants(sandbox, want.group, want.name, verbDelete) {
			t.Errorf("sandbox writer Role cannot create and delete %s", want.name)
		}
	}
	// Named logins' claims are created on first use and never deleted.
	claims := apiResource{"", "persistentvolumeclaims"}
	if !grants(sandbox, claims.group, claims.name, verbCreate) || grants(sandbox, claims.group, claims.name, verbDelete) {
		t.Error("sandbox writer Role must create, and never delete, login claims")
	}
	for _, verb := range []string{verbCreate, verbUpdate, verbPatch, verbDelete} {
		if grants(cluster, claims.group, claims.name, verb) {
			t.Errorf("controller ClusterRole grants %s on claims in every namespace", verb)
		}
	}
	// Existing run NetworkPolicies are reconciled in place.
	if !grants(sandbox, networkPolicies.group, networkPolicies.name, verbUpdate) {
		t.Error("sandbox writer Role cannot update an existing run NetworkPolicy")
	}
	// The Pod watch and the cached ServiceAccount and NetworkPolicy reads need
	// cluster-wide informers; managed runs read their registry ConfigMap in
	// the run namespace.
	informer := []string{verbList, "watch"}
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
