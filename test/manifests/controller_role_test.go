package manifests_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	sigyaml "sigs.k8s.io/yaml"
)

// grantsWithWildcards reports whether rules grant verb on resource in group,
// counting "*" in any of the three positions. Subresources such as
// agentruns/status are distinct resources; only "*" or the exact name matches.
func grantsWithWildcards(rules []rbacv1.PolicyRule, group, resource, verb string) bool {
	has := func(list []string, want string) bool {
		return slices.Contains(list, want) || slices.Contains(list, rbacv1.ResourceAll)
	}
	for _, rule := range rules {
		if len(rule.ResourceNames) == 0 && has(rule.APIGroups, group) && has(rule.Resources, resource) &&
			has(rule.Verbs, verb) {
			return true
		}
	}
	return false
}

// TestInstalledControllerRoleCoversGeneratedRole guards the AGENTS.md rule that
// a +kubebuilder:rbac marker change must also reach the hand-maintained role
// the controller runs with: config/rbac/role.yaml is generated and not
// installed. The installed role may grant more.
func TestInstalledControllerRoleCoversGeneratedRole(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var generated rbacv1.ClusterRole
	if err := sigyaml.Unmarshal(data, &generated); err != nil {
		t.Fatal(err)
	}
	if len(generated.Rules) == 0 {
		t.Fatal("generated config/rbac/role.yaml has no rules")
	}
	installed := installedControllerClusterRules(t)
	for _, rule := range generated.Rules {
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				for _, verb := range rule.Verbs {
					if !grantsWithWildcards(installed, group, resource, verb) {
						t.Errorf("generated role grants %q on %q (group %q) but the installed controller ClusterRole does not;"+
							" change config/rbac/controller_manager_role.yaml too", verb, resource, group)
					}
				}
			}
		}
	}
}
