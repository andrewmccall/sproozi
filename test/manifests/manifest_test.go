package manifests_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	sigyaml "sigs.k8s.io/yaml"
)

const (
	controllerManagerName      = "sproozi-controller-manager"
	sharedGatewayName          = "sproozi-gateway"
	webhookName                = "sproozi-webhook"
	retiredModelGatewayName    = "sproozi-model-gateway"
	retiredCapabilityProxyName = "sproozi-capability-proxy"
	retiredEgressGatewayName   = "sproozi-egress-gateway"
	systemNamespace            = "sproozi-system"
)

func TestSampleRuntimeUsesAdministratorOwnedPodTemplate(t *testing.T) {
	path := filepath.Join(repoRoot(t), "config", "samples", "sproozi_v1alpha1_agentruntime.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "podTemplate:") {
		t.Fatal("sample runtime must define an administrator-owned podTemplate")
	}
	if !strings.Contains(text, "workloadContainers: [agent]") {
		t.Fatal("sample runtime must explicitly select its workload container")
	}
	if !strings.Contains(text, "image: ghcr.io/openai/codex-universal@sha256:") {
		t.Fatal("sample runtime must use a digest-pinned vanilla Codex image")
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func renderDefault(t *testing.T) []map[string]any {
	return renderKustomization(t, filepath.Join(repoRoot(t), "config", "default"))
}

func renderInstall(t *testing.T) []map[string]any {
	return renderKustomization(t, filepath.Join(repoRoot(t), "config", "install"))
}

func renderKustomization(t *testing.T, path string) []map[string]any {
	t.Helper()
	root := repoRoot(t)

	makeCmd := exec.Command("make", "kustomize")
	makeCmd.Dir = root
	if out, err := makeCmd.CombinedOutput(); err != nil {
		t.Fatalf("make kustomize: %v\n%s", err, out)
	}

	buildCmd := exec.Command(filepath.Join(root, "bin", "kustomize"), "build", path)
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

func objectsOfKind(docs []map[string]any, kind string) []map[string]any {
	var out []map[string]any
	for _, doc := range docs {
		if doc["kind"] == kind {
			out = append(out, doc)
		}
	}
	return out
}

func metadataName(doc map[string]any) string {
	metadata, _ := doc["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	return name
}

func metadataNamespace(doc map[string]any) string {
	metadata, _ := doc["metadata"].(map[string]any)
	namespace, _ := metadata["namespace"].(string)
	return namespace
}

func TestRenderedDefaultIncludesOnlySharedGateway(t *testing.T) {
	docs := renderDefault(t)
	names := make([]string, 0, len(docs))
	for _, doc := range objectsOfKind(docs, "Deployment") {
		names = append(names, metadataName(doc))
	}

	for _, want := range []string{
		controllerManagerName,
		sharedGatewayName,
		webhookName,
	} {
		if !slices.Contains(names, want) {
			t.Fatalf("expected rendered deployment %q, got %v", want, names)
		}
	}
	for _, retired := range []string{
		retiredModelGatewayName,
		retiredCapabilityProxyName,
		retiredEgressGatewayName,
	} {
		if slices.Contains(names, retired) {
			t.Fatalf("retired per-capability gateway %q must not be rendered; got %v", retired, names)
		}
	}
}

func TestRenderedDefaultWiresAdministratorModelPricingIntoSharedGateway(t *testing.T) {
	docs := renderDefault(t)
	var pricing map[string]any
	for _, doc := range objectsOfKind(docs, "ConfigMap") {
		if metadataName(doc) != "sproozi-model-gateway-pricing" {
			continue
		}
		data, _ := doc["data"].(map[string]any)
		pricingJSON, _ := data["pricing.json"].(string)
		if err := json.Unmarshal([]byte(pricingJSON), &pricing); err != nil {
			t.Fatalf("administrator pricing is not JSON: %v", err)
		}
		break
	}
	if pricing == nil {
		t.Fatal("rendered default must include the administrator-owned model pricing ConfigMap")
	}
	models, _ := pricing["models"].(map[string]any)
	demoModel, _ := models["gpt-6.1-sol"].(map[string]any)
	if demoModel["inputMicrosPerMillionTokens"] != float64(2_000_000) ||
		demoModel["cachedInputMicrosPerMillionTokens"] != float64(100_000) ||
		demoModel["outputMicrosPerMillionTokens"] != float64(10_000_000) {
		t.Fatalf("administrator pricing for demo model = %v", demoModel)
	}
	if _, retained := models["gpt-5.3-codex"]; !retained {
		t.Fatal("model migration removed existing API-key pricing")
	}

	for _, doc := range objectsOfKind(docs, "Deployment") {
		if metadataName(doc) != sharedGatewayName {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		podSpec, _ := template["spec"].(map[string]any)
		containers, _ := podSpec["containers"].([]any)
		container, _ := containers[0].(map[string]any)
		env, _ := container["env"].([]any)
		foundPath := false
		for _, raw := range env {
			entry, _ := raw.(map[string]any)
			if entry["name"] == "MODEL_PRICING_PATH" && entry["value"] == "/etc/sproozi/model/pricing.json" {
				foundPath = true
			}
		}
		if !foundPath {
			t.Fatal("shared gateway must load pricing from its mounted administrator-owned file")
		}
		volumes, _ := podSpec["volumes"].([]any)
		for _, raw := range volumes {
			volume, _ := raw.(map[string]any)
			if volume["name"] != "model-pricing" {
				continue
			}
			configMap, _ := volume["configMap"].(map[string]any)
			if configMap["name"] != "sproozi-model-gateway-pricing" {
				t.Fatalf("shared gateway model-pricing volume = %v", configMap)
			}
			return
		}
		t.Fatal("shared gateway must mount the administrator-owned model pricing ConfigMap")
	}
	t.Fatal("shared gateway deployment was not rendered")
}

func TestRenderedDefaultManagerHasNoRemovedLifecycleConfiguration(t *testing.T) {
	for _, doc := range objectsOfKind(renderDefault(t), "Deployment") {
		if metadataName(doc) != controllerManagerName {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		podSpec, _ := template["spec"].(map[string]any)
		containers, _ := podSpec["containers"].([]any)
		container, _ := containers[0].(map[string]any)
		env, _ := container["env"].([]any)
		for _, raw := range env {
			entry, _ := raw.(map[string]any)
			if entry["name"] == "REPLAY_NAMESPACE" {
				t.Fatal("controller manager must not configure webhook state")
			}
		}
		return
	}
	t.Fatal("controller manager deployment was not rendered")
}

func TestRenderedDefaultIncludesCurrentServiceAccounts(t *testing.T) {
	docs := renderDefault(t)
	names := make([]string, 0, len(docs))
	for _, doc := range objectsOfKind(docs, "ServiceAccount") {
		names = append(names, metadataName(doc))
	}

	for _, want := range []string{
		controllerManagerName,
		"sproozi-shared-gateway",
		webhookName,
	} {
		if !slices.Contains(names, want) {
			t.Fatalf("expected rendered service account %q, got %v", want, names)
		}
	}
}

func TestSREDemoUsesSharedGatewayContract(t *testing.T) {
	docs := renderKustomization(t, filepath.Join(repoRoot(t), "examples", "sre-demo"))
	if secrets := objectsOfKind(docs, "Secret"); len(secrets) != 0 {
		names := make([]string, 0, len(secrets))
		for _, secret := range secrets {
			names = append(names, metadataName(secret))
		}
		t.Fatalf("SRE example must not commit credentials as Secret objects; got %v", names)
	}

	runtimes := objectsOfKind(docs, "AgentRuntime")
	if len(runtimes) != 1 {
		t.Fatalf("expected one AgentRuntime, got %d", len(runtimes))
	}
	spec, _ := runtimes[0]["spec"].(map[string]any)
	if spec["gatewayEndpoint"] != "https://sproozi-gateway.sproozi-system.svc:8443" {
		t.Fatalf("unexpected gateway endpoint: %v", spec["gatewayEndpoint"])
	}

	clientConfig, _ := spec["clientConfig"].(map[string]any)
	trustBundle, _ := clientConfig["trustBundleConfigMap"].(map[string]any)
	if trustBundle["name"] != "sproozi-sandbox-trust" || trustBundle["key"] != "ca-bundle.pem" {
		t.Fatalf("unexpected trust bundle reference: %v", trustBundle)
	}
}

func TestSREDemoRuntimePassesRuntimeValidation(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "examples", "sre-demo", "agentruntime.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var agentRuntime sprooziv1alpha1.AgentRuntime
	if err := sigyaml.Unmarshal(data, &agentRuntime); err != nil {
		t.Fatal(err)
	}
	if err := agentRuntime.Spec.ValidateRuntimeTemplate(); err != nil {
		t.Fatalf("SRE demo runtime is not admissible: %v", err)
	}
}

func TestInstallBundleIncludesWebhookReplayIsolation(t *testing.T) {
	docs := renderInstall(t)
	foundNamespace := false
	foundReplayBinding := false
	for _, doc := range objectsOfKind(docs, "Namespace") {
		foundNamespace = foundNamespace || metadataName(doc) == "sproozi-webhook-state"
	}
	for _, doc := range objectsOfKind(docs, "RoleBinding") {
		if metadataName(doc) != "sproozi-webhook-replay" || metadataNamespace(doc) != "sproozi-webhook-state" {
			continue
		}
		subjects, _ := doc["subjects"].([]any)
		if len(subjects) == 1 {
			subject, _ := subjects[0].(map[string]any)
			foundReplayBinding = subject["name"] == webhookName && subject["namespace"] == systemNamespace
		}
	}
	if !foundNamespace || !foundReplayBinding {
		t.Fatalf("install bundle must create webhook replay state and bind the webhook (namespace=%v binding=%v)",
			foundNamespace,
			foundReplayBinding)
	}
}

// TestInstallBundleScopesWebhookSecretReads verifies that the webhook can read
// Secrets only in the HMAC Secret namespace. sproozi-system holds the provider
// and GitHub credentials, so no role bound to the webhook may grant Secret
// access there or cluster-wide.
func TestInstallBundleScopesWebhookSecretReads(t *testing.T) {
	const (
		secretNamespace = "sproozi-webhooks"
		roleBindingKind = "RoleBinding"
		getVerb         = "get"
	)
	docs := renderInstall(t)
	roles := map[string][]rbacv1.PolicyRule{}
	for _, kind := range []string{"Role", "ClusterRole"} {
		for _, doc := range objectsOfKind(docs, kind) {
			var role rbacv1.ClusterRole
			decodeObject(t, doc, &role)
			roles[kind+"/"+metadataNamespace(doc)+"/"+role.Name] = role.Rules
		}
	}
	foundNamespace := false
	for _, doc := range objectsOfKind(docs, "Namespace") {
		foundNamespace = foundNamespace || metadataName(doc) == secretNamespace
	}
	foundSecretRead, foundTemplateRead := false, false
	for _, kind := range []string{roleBindingKind, "ClusterRoleBinding"} {
		for _, doc := range objectsOfKind(docs, kind) {
			var binding rbacv1.RoleBinding
			decodeObject(t, doc, &binding)
			if !slices.ContainsFunc(binding.Subjects, func(s rbacv1.Subject) bool {
				return s.Kind == rbacv1.ServiceAccountKind && s.Name == webhookName && s.Namespace == systemNamespace
			}) {
				continue
			}
			roleNamespace := binding.Namespace
			if binding.RoleRef.Kind == "ClusterRole" {
				roleNamespace = ""
			}
			rules, ok := roles[binding.RoleRef.Kind+"/"+roleNamespace+"/"+binding.RoleRef.Name]
			if !ok {
				t.Fatalf("%s %s binds the webhook to missing %s %s", kind, binding.Name, binding.RoleRef.Kind, binding.RoleRef.Name)
			}
			for _, rule := range rules {
				if binding.Namespace == systemNamespace && slices.Contains(rule.Resources, "agenttemplates") {
					foundTemplateRead = true
				}
				if !slices.Contains(rule.Resources, "secrets") && !slices.Contains(rule.Resources, "*") {
					continue
				}
				if kind != roleBindingKind || binding.Namespace != secretNamespace ||
					!slices.Equal(rule.Verbs, []string{getVerb}) || len(rule.Resources) != 1 {
					t.Fatalf("%s %s grants the webhook Secret access outside %s: %+v", kind, binding.Name, secretNamespace, rule)
				}
				foundSecretRead = true
			}
		}
	}
	// The rendered webhook ServiceAccount must also be bound to its template
	// role, or this check would pass for a webhook with no bindings at all.
	if !foundNamespace || !foundSecretRead || !foundTemplateRead {
		t.Fatalf("install bundle must create %s, let the webhook get Secrets there and read templates in %s "+
			"(namespace=%v secrets=%v templates=%v)",
			secretNamespace, systemNamespace, foundNamespace, foundSecretRead, foundTemplateRead)
	}
}

func decodeObject(t *testing.T, doc map[string]any, into any) {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatal(err)
	}
}

func TestSREDemoRendererRequiresAndAppliesImmutableInputs(t *testing.T) {
	root := repoRoot(t)
	image := "registry.example/codex@sha256:" + strings.Repeat("b", 64)
	cmd := exec.Command("python3",
		filepath.Join(root,
			"hack",
			"demo",
			"render.py"),
		"--codex-image",
		image,
		"--repository",
		"example/demo")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("render SRE demo: %v\n%s", err, out)
	}
	text := string(out)
	if strings.Count(text, image) != 1 || strings.Count(text, "example/demo") != 3 {
		t.Fatalf("renderer did not apply exact immutable inputs")
	}
	if strings.Contains(text, strings.Repeat("a", 64)) || strings.Contains(text, "example/sproozi-demo") {
		t.Fatal("renderer left a checked-in placeholder in runnable output")
	}
}

func TestKindDemoDisablesDefaultCNI(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "hack", "kind-cluster.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "disableDefaultCNI: true") {
		t.Fatal("Kind demo cluster must disable kindnet before installing the selected enforcing CNI")
	}
}

func TestSharedGatewayRBACOwnsOnlyBoundedKubernetesReads(t *testing.T) {
	docs := renderDefault(t)
	var role map[string]any
	for _, doc := range objectsOfKind(docs, "ClusterRole") {
		if metadataName(doc) == "sproozi-shared-gateway" {
			role = doc
			break
		}
	}
	if role == nil {
		t.Fatal("shared-gateway ClusterRole was not rendered")
	}
	rules, _ := role["rules"].([]any)
	allowed := map[string]bool{"pods": true,
		"pods/log":        true,
		"events":          true,
		"services":        true,
		"deployments":     true,
		"replicasets":     true,
		"statefulsets":    true,
		"serviceaccounts": true}
	for _, raw := range rules {
		rule, _ := raw.(map[string]any)
		resources, _ := rule["resources"].([]any)
		verbs, _ := rule["verbs"].([]any)
		for _, rawResource := range resources {
			resource, _ := rawResource.(string)
			if resource == "tokenreviews" ||
				resource == "agentruns" ||
				resource == "agentpolicies" ||
				resource == "agenttemplates" {
				continue
			}
			if !allowed[resource] {
				t.Fatalf("shared-gateway ClusterRole grants unexpected Kubernetes resource %q", resource)
			}
			for _, rawVerb := range verbs {
				verb, _ := rawVerb.(string)
				if verb != "get" && verb != "list" && verb != "watch" {
					t.Fatalf("shared-gateway ClusterRole grants unexpected Kubernetes verb %q", verb)
				}
			}
		}
	}
}

func TestSharedGatewayHasNamespacedDurableBudgetRBAC(t *testing.T) {
	docs := renderDefault(t)
	foundRole := false
	foundNamespaceEnv := false
	for _, doc := range objectsOfKind(docs, "Role") {
		if metadataName(doc) != "sproozi-shared-gateway-budget" || metadataNamespace(doc) != systemNamespace {
			continue
		}
		foundRole = true
		rules, _ := doc["rules"].([]any)
		var serialized strings.Builder
		for _, rule := range rules {
			fmt.Fprint(&serialized, rule)
		}
		for _, want := range []string{"configmaps", "get", "create", "update"} {
			if !strings.Contains(serialized.String(), want) {
				t.Fatalf("budget Role is missing %q: %v", want, rules)
			}
		}
	}
	for _, doc := range objectsOfKind(docs, "Deployment") {
		if metadataName(doc) != sharedGatewayName {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		podSpec, _ := template["spec"].(map[string]any)
		containers, _ := podSpec["containers"].([]any)
		container, _ := containers[0].(map[string]any)
		env, _ := container["env"].([]any)
		for _, raw := range env {
			entry, _ := raw.(map[string]any)
			foundNamespaceEnv = foundNamespaceEnv || (entry["name"] == "BUDGET_NAMESPACE" && entry["value"] == systemNamespace)
		}
	}
	if !foundRole || !foundNamespaceEnv {
		t.Fatalf("shared gateway durable budget configuration missing (role=%v env=%v)", foundRole, foundNamespaceEnv)
	}
}

func TestRenderedDeploymentsUseDistinctServiceAccounts(t *testing.T) {
	docs := renderDefault(t)

	want := map[string]string{
		controllerManagerName:      controllerManagerName,
		retiredModelGatewayName:    retiredModelGatewayName,
		retiredCapabilityProxyName: retiredCapabilityProxyName,
		retiredEgressGatewayName:   retiredEgressGatewayName,
		webhookName:                webhookName,
	}

	for _, doc := range objectsOfKind(docs, "Deployment") {
		name := metadataName(doc)
		spec, _ := doc["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		podSpecHolder, _ := template["spec"].(map[string]any)
		got, _ := podSpecHolder["serviceAccountName"].(string)

		if expected, ok := want[name]; ok && got != expected {
			t.Fatalf("expected deployment %q to use service account %q, got %q", name, expected, got)
		}
	}
}

func TestRenderedDefaultIncludesSystemNetworkPolicies(t *testing.T) {
	docs := renderDefault(t)
	names := make([]string, 0, len(docs))
	for _, doc := range objectsOfKind(docs, "NetworkPolicy") {
		names = append(names, metadataName(doc))
	}

	for _, want := range []string{
		"sproozi-default-deny-ingress",
		"sproozi-allow-agent-traffic",
		"sproozi-allow-webhook-traffic",
	} {
		if !slices.Contains(names, want) {
			t.Fatalf("expected rendered network policy %q, got %v", want, names)
		}
	}
}
