package manifests_test

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// grant is one RBAC rule as it applies to a subject: namespace "" means
// cluster-wide. A rule with resourceNames grants its verbs on those names only.
type grant struct {
	namespace                                  string
	apiGroups, resources, verbs, resourceNames []string
}

func stringsOf(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// builtInClusterRoles are the Kubernetes bootstrap ClusterRoles a rendered
// binding may refer to without the manifests defining them, with the rules
// the API server installs for them. Any other roleRef that does not resolve
// fails the test, so a binding to a broad role such as edit or admin cannot
// pass unexamined.
var builtInClusterRoles = map[string][]grant{
	// system:auth-delegator: create TokenReviews and SubjectAccessReviews, so
	// a server can authenticate and authorize its callers. It reads no objects.
	"system:auth-delegator": {
		{apiGroups: []string{"authentication.k8s.io"}, resources: []string{resourceTokenReviews},
			verbs: []string{verbCreate}},
		{apiGroups: []string{"authorization.k8s.io"}, resources: []string{"subjectaccessreviews"},
			verbs: []string{verbCreate}},
	},
}

// grantsFor resolves every rule bound to one ServiceAccount, following
// RoleBindings to Roles or ClusterRoles and ClusterRoleBindings to
// ClusterRoles, the way the API server's authorizer does. It matches the
// ServiceAccount by name and through the groups every ServiceAccount token
// carries, and fails on a roleRef it cannot resolve.
func grantsFor(t *testing.T, docs []map[string]any, saNamespace, saName string) []grant {
	t.Helper()
	grants, err := resolveGrants(docs, saNamespace, saName)
	if err != nil {
		t.Fatal(err)
	}
	return grants
}

func resolveGrants(docs []map[string]any, saNamespace, saName string) ([]grant, error) {
	rulesOf := func(kind, namespace, name string) ([]map[string]any, bool) {
		for _, doc := range objectsOfKind(docs, kind) {
			if metadataName(doc) == name && (kind == kindClusterRole || metadataNamespace(doc) == namespace) {
				raw, _ := doc["rules"].([]any)
				rules := make([]map[string]any, 0, len(raw))
				for _, r := range raw {
					rules = append(rules, r.(map[string]any))
				}
				return rules, true
			}
		}
		return nil, false
	}
	groups := []string{"system:serviceaccounts", "system:serviceaccounts:" + saNamespace, "system:authenticated"}
	user := "system:serviceaccount:" + saNamespace + ":" + saName
	bound := func(binding map[string]any) bool {
		subjects, _ := binding["subjects"].([]any)
		for _, raw := range subjects {
			s := raw.(map[string]any)
			name, _ := s[keyName].(string)
			switch s[keyKind] {
			case "ServiceAccount":
				namespace, _ := s[keyNamespace].(string)
				if namespace == "" {
					namespace = metadataNamespace(binding)
				}
				if name == saName && namespace == saNamespace {
					return true
				}
			case "Group":
				if slices.Contains(groups, name) {
					return true
				}
			case "User":
				if name == user {
					return true
				}
			}
		}
		return false
	}
	var out []grant
	for _, kind := range []string{kindRoleBinding, "ClusterRoleBinding"} {
		for _, binding := range objectsOfKind(docs, kind) {
			if !bound(binding) {
				continue
			}
			ref := binding["roleRef"].(map[string]any)
			refKind, refName := ref[keyKind].(string), ref[keyName].(string)
			scope := ""
			if kind == kindRoleBinding {
				scope = metadataNamespace(binding)
			}
			rules, found := rulesOf(refKind, scope, refName)
			if !found {
				builtIn, known := builtInClusterRoles[refName]
				if refKind != kindClusterRole || !known {
					return nil, fmt.Errorf("%s %s/%s refers to %s %s, which the manifests do not define "+
						"and which is not a known built-in role",
						kind, metadataNamespace(binding), metadataName(binding), refKind, refName)
				}
				for _, g := range builtIn {
					g.namespace = scope
					out = append(out, g)
				}
				continue
			}
			for _, rule := range rules {
				out = append(out, grant{namespace: scope, apiGroups: stringsOf(rule["apiGroups"]),
					resources: stringsOf(rule["resources"]), verbs: stringsOf(rule["verbs"]),
					resourceNames: stringsOf(rule["resourceNames"])})
			}
		}
	}
	return out, nil
}

func grantMatches(g grant, namespace, group, resource, verb string) bool {
	match := func(list []string, want string) bool {
		return slices.Contains(list, want) || slices.Contains(list, "*")
	}
	return (g.namespace == "" || g.namespace == namespace) && match(g.apiGroups, group) &&
		match(g.resources, resource) && match(g.verbs, verb)
}

// allows reports whether the grants allow verb on every object of the
// resource: a rule limited by resourceNames does not count.
func allows(grants []grant, namespace, group, resource, verb string) bool {
	for _, g := range grants {
		if len(g.resourceNames) == 0 && grantMatches(g, namespace, group, resource, verb) {
			return true
		}
	}
	return false
}

// allowsNamed reports whether the grants allow verb on the named object.
func allowsNamed(grants []grant, namespace, group, resource, verb, name string) bool {
	for _, g := range grants {
		if grantMatches(g, namespace, group, resource, verb) &&
			(len(g.resourceNames) == 0 || slices.Contains(g.resourceNames, name)) {
			return true
		}
	}
	return false
}

// allowsAny reports whether the grants allow verb on at least one object of
// the resource, named or not. Use it to prove an identity has no such access.
func allowsAny(grants []grant, namespace, resource, verb string) bool {
	for _, g := range grants {
		if grantMatches(g, namespace, "", resource, verb) {
			return true
		}
	}
	return false
}

var configMapWrites = []string{verbCreate, verbUpdate, verbPatch, verbDelete, "deletecollection"}

// The documented managed-session deployment: the install, the sandbox
// namespace, the remote executor example and CLI access.
func managedSessionDocs(t *testing.T) []map[string]any {
	t.Helper()
	root := repoRoot(t)
	paths := []string{
		filepath.Join(root, "config", "install"),
		filepath.Join(root, "config", "agent-rbac"),
		filepath.Join(root, "config", "cli-access"),
		filepath.Join(root, "examples", "portable-workflows", "kubernetes"),
	}
	docs := make([]map[string]any, 0, 256)
	for _, path := range paths {
		docs = append(docs, renderKustomization(t, path)...)
	}
	return docs
}

// No identity other than the controller can write the gateway's
// configuration ConfigMaps in sproozi-system (ADR 0041). A writer there could
// repoint an MCP registration and receive its bearer token after a restart.
// A write limited to named ConfigMaps still counts.
func TestOnlyTheControllerWritesSystemConfigMaps(t *testing.T) {
	docs := managedSessionDocs(t)
	for _, sa := range [][2]string{
		{systemNamespace, gatewayAccount},
		{sessionsNamespace, "sproozi-work"},
		{sessionsNamespace, "sproozi-cli"},
	} {
		grants := grantsFor(t, docs, sa[0], sa[1])
		for _, verb := range configMapWrites {
			if allowsAny(grants, systemNamespace, resourceConfigMaps, verb) {
				t.Errorf("%s/%s can %s ConfigMaps in %s", sa[0], sa[1], verb, systemNamespace)
			}
		}
		if allowsAny(grants, systemNamespace, "pods/portforward", verbCreate) {
			t.Errorf("%s/%s can port-forward into %s", sa[0], sa[1], systemNamespace)
		}
	}
}

// Every API call on a managed session's path is authorized for the identity
// that makes it: the executor registers runs, reads their templates, initializes
// and seals their accounts, execs into and fences their Pods, aborts runs and
// reviews the CLI's token; the gateway reads registrations and charges the same
// accounts; the CLI discovers and reaches the executor.
func TestManagedSessionPathIsAuthorized(t *testing.T) {
	docs := managedSessionDocs(t)
	executor := grantsFor(t, docs, sessionsNamespace, "sproozi-work")
	gateway := grantsFor(t, docs, systemNamespace, gatewayAccount)
	cli := grantsFor(t, docs, sessionsNamespace, "sproozi-cli")
	for _, check := range []struct {
		who                              string
		grants                           []grant
		namespace, group, resource, verb string
	}{
		{whoExecutor, executor, sessionsNamespace, "", resourceConfigMaps, verbGet},
		{whoExecutor, executor, sessionsNamespace, "", resourceConfigMaps, verbCreate},
		{whoExecutor, executor, sessionsNamespace, "", resourceConfigMaps, verbUpdate},
		{whoExecutor, executor, sessionsNamespace, groupSproozi, resourceAgentRuns, verbGet},
		{whoExecutor, executor, sessionsNamespace, groupSproozi, resourceAgentRuns, verbCreate},
		// Abort cancels a run by patching it.
		{whoExecutor, executor, sessionsNamespace, groupSproozi, resourceAgentRuns, verbPatch},
		{whoExecutor, executor, sessionsNamespace, groupSproozi, "agenttemplates", verbGet},
		{whoExecutor, executor, sessionsNamespace, groupSproozi, "agentpolicies", verbGet},
		{whoExecutor, executor, sessionsNamespace, groupSproozi, "agentruntimes", verbGet},
		{whoExecutor, executor, budgetNamespace, groupSproozi, resourceBudgetLedgers, verbGet},
		{whoExecutor, executor, budgetNamespace, groupSproozi, resourceBudgetLedgers, verbCreate},
		{whoExecutor, executor, budgetNamespace, groupSproozi, resourceBudgetLedgers, verbUpdate},
		{whoExecutor, executor, agentsNamespace, "", resourcePods, verbGet},
		// Releasing the writer fence patches the sandbox Pod; abort deletes it.
		{whoExecutor, executor, agentsNamespace, "", resourcePods, verbPatch},
		{whoExecutor, executor, agentsNamespace, "", resourcePods, verbDelete},
		{whoExecutor, executor, agentsNamespace, "", "pods/exec", verbCreate},
		{whoExecutor, executor, "", "authentication.k8s.io", resourceTokenReviews, verbCreate},
		{whoExecutor, executor, "", "authorization.k8s.io", "subjectaccessreviews", verbCreate},
		{whoGateway, gateway, sessionsNamespace, "", resourceConfigMaps, verbGet},
		{whoGateway, gateway, sessionsNamespace, groupSproozi, resourceAgentRuns, verbGet},
		{whoGateway, gateway, budgetNamespace, groupSproozi, resourceBudgetLedgers, verbUpdate},
		{whoGateway, gateway, budgetNamespace, groupSproozi, resourceBudgetLedgers, verbList},
		{whoGateway, gateway, budgetNamespace, groupSproozi, resourceBudgetLedgers, verbDelete},
		{whoCLI, cli, sessionsNamespace, "", "services", verbList},
		{whoCLI, cli, sessionsNamespace, "discovery.k8s.io", "endpointslices", verbList},
		{whoCLI, cli, sessionsNamespace, "", "pods/portforward", verbCreate},
		{whoCLI, cli, sessionsNamespace, groupSproozi, "remoteexecutors", "use"},
	} {
		if !allows(check.grants, check.namespace, check.group, check.resource, check.verb) {
			t.Errorf("%s cannot %s %s/%s in %s", check.who, check.verb, check.group, check.resource, check.namespace)
		}
	}
	if !allowsNamed(cli, sessionsNamespace, "", resourceConfigMaps, verbGet, "sproozi-remote-executor") {
		t.Error("cli cannot read the executor's CA ConfigMap sproozi-remote-executor")
	}
	if allows(cli, sessionsNamespace, "", resourceConfigMaps, verbGet) {
		t.Error("cli can read every ConfigMap in sproozi-sessions; it needs only sproozi-remote-executor")
	}
	for _, verb := range []string{verbCreate, verbUpdate, verbPatch, verbDelete} {
		if allowsAny(gateway, sessionsNamespace, resourceConfigMaps, verb) {
			t.Errorf("gateway can %s registrations in sproozi-sessions; it only reads them", verb)
		}
	}
}

func testGrantDocs(roleRules, subjects string) []map[string]any {
	return []map[string]any{
		{keyKind: "Role", keyMetadata: map[string]any{keyName: "r", keyNamespace: "ns"},
			"rules": []any{map[string]any{"apiGroups": []any{""}, "resources": []any{resourceConfigMaps},
				"verbs": []any{verbGet}, "resourceNames": stringList(roleRules)}}},
		{keyKind: kindRoleBinding, keyMetadata: map[string]any{keyName: "b", keyNamespace: "ns"},
			"roleRef":  map[string]any{keyKind: "Role", keyName: "r"},
			"subjects": []any{map[string]any{keyKind: "Group", keyName: subjects}}},
	}
}

func stringList(csv string) []any {
	if csv == "" {
		return nil
	}
	out := []any{}
	for s := range strings.SplitSeq(csv, ",") {
		out = append(out, s)
	}
	return out
}

// The evaluator itself: resourceNames narrow a rule, and Group subjects that
// every ServiceAccount token carries bind it.
func TestGrantsForHonoursResourceNamesAndGroups(t *testing.T) {
	for _, group := range []string{"system:serviceaccounts", "system:serviceaccounts:sa-ns", "system:authenticated"} {
		grants := grantsFor(t, testGrantDocs("", group), "sa-ns", "sa")
		if !allows(grants, "ns", "", resourceConfigMaps, verbGet) {
			t.Errorf("group %s did not bind the ServiceAccount", group)
		}
	}
	if grants := grantsFor(t, testGrantDocs("", "system:serviceaccounts:other"), "sa-ns", "sa"); len(grants) != 0 {
		t.Errorf("another namespace's ServiceAccount group bound the ServiceAccount: %v", grants)
	}
	named := grantsFor(t, testGrantDocs("one", "system:authenticated"), "sa-ns", "sa")
	if allows(named, "ns", "", resourceConfigMaps, verbGet) {
		t.Error("a rule with resourceNames granted the verb on every name")
	}
	if !allowsNamed(named, "ns", "", resourceConfigMaps, verbGet, "one") ||
		allowsNamed(named, "ns", "", resourceConfigMaps, verbGet, "two") {
		t.Error("allowsNamed did not follow resourceNames")
	}
	if !allowsAny(named, "ns", resourceConfigMaps, verbGet) {
		t.Error("allowsAny ignored a rule with resourceNames")
	}
}

// A binding to a role the manifests do not define fails unless it is a known
// built-in, whose rules are then applied.
func TestGrantsForRejectsUnresolvedRoleRefs(t *testing.T) {
	binding := func(name string) []map[string]any {
		return []map[string]any{{keyKind: "ClusterRoleBinding", keyMetadata: map[string]any{keyName: "b"},
			"roleRef":  map[string]any{keyKind: kindClusterRole, keyName: name},
			"subjects": []any{map[string]any{keyKind: "ServiceAccount", keyName: "sa", keyNamespace: "sa-ns"}}}}
	}
	if _, err := resolveGrants(binding("edit"), "sa-ns", "sa"); err == nil {
		t.Error("a binding to the built-in edit ClusterRole resolved silently")
	}
	grants, err := resolveGrants(binding("system:auth-delegator"), "sa-ns", "sa")
	if err != nil || !allows(grants, "", "authentication.k8s.io", resourceTokenReviews, verbCreate) {
		t.Errorf("system:auth-delegator did not resolve to its rules: %v %v", grants, err)
	}
}
