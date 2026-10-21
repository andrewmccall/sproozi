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
	"sync"
	"testing"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/placement"
	"k8s.io/apimachinery/pkg/util/yaml"
	sigyaml "sigs.k8s.io/yaml"
)

const (
	controllerManagerName      = "sproozi-controller-manager"
	sharedGatewayName          = "sproozi-gateway"
	budgetNamespace            = "sproozi-system"
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

// renders holds each kustomization's build output for the package run, so
// tests share one kustomize build per path. Callers decode their own copy.
var (
	kustomizeOnce sync.Once
	kustomizeErr  error
	renders       sync.Map // path -> *render
)

type render struct {
	once sync.Once
	out  []byte
	err  error
}

func renderKustomization(t *testing.T, path string) []map[string]any {
	t.Helper()
	root := repoRoot(t)

	kustomizeOnce.Do(func() {
		makeCmd := exec.Command("make", "kustomize")
		makeCmd.Dir = root
		if out, err := makeCmd.CombinedOutput(); err != nil {
			kustomizeErr = fmt.Errorf("make kustomize: %w\n%s", err, out)
		}
	})
	if kustomizeErr != nil {
		t.Fatal(kustomizeErr)
	}

	cached, _ := renders.LoadOrStore(path, &render{})
	r := cached.(*render)
	r.once.Do(func() {
		buildCmd := exec.Command(filepath.Join(root, "bin", "kustomize"), "build", path)
		buildCmd.Dir = root
		out, err := buildCmd.CombinedOutput()
		if err != nil {
			r.err = fmt.Errorf("kustomize build: %w\n%s", err, out)
		}
		r.out = out
	})
	if r.err != nil {
		t.Fatal(r.err)
	}

	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(r.out), 4096)
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
		if doc[keyKind] == kind {
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
				t.Fatal("controller manager must not configure the removed webhook's state")
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
		gatewayAccount,
	} {
		if !slices.Contains(names, want) {
			t.Fatalf("expected rendered service account %q, got %v", want, names)
		}
	}

	// Each component runs as its own identity rather than a shared one.
	for _, doc := range objectsOfKind(docs, "Deployment") {
		name := metadataName(doc)
		if name != controllerManagerName {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		podSpec, _ := template["spec"].(map[string]any)
		if got, _ := podSpec["serviceAccountName"].(string); got != name {
			t.Fatalf("expected deployment %q to use service account %q, got %q", name, name, got)
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

// The event webhook was removed (ADR 0043). Nothing in the install may bring
// back its Deployment, identity, network exception or state namespaces.
func TestInstallBundleHasNoEventWebhook(t *testing.T) {
	for _, doc := range renderInstall(t) {
		kind, _ := doc[keyKind].(string)
		name := metadataName(doc)
		if strings.Contains(name, "webhook") && kind != "CustomResourceDefinition" {
			t.Fatalf("install bundle still renders %s %s", kind, name)
		}
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
		"octo-org/sproozi-demo")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("render SRE demo: %v\n%s", err, out)
	}
	text := string(out)
	if strings.Count(text, image) != 1 || strings.Count(text, "octo-org/sproozi-demo") != 3 {
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
	for _, doc := range objectsOfKind(docs, kindClusterRole) {
		if metadataName(doc) == gatewayAccount {
			role = doc
			break
		}
	}
	if role == nil {
		t.Fatal("shared-gateway ClusterRole was not rendered")
	}
	rules, _ := role["rules"].([]any)
	allowed := map[string]bool{resourcePods: true,
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
			if resource == resourceTokenReviews ||
				resource == resourceAgentRuns ||
				resource == "agentpolicies" ||
				resource == "agenttemplates" {
				continue
			}
			if !allowed[resource] {
				t.Fatalf("shared-gateway ClusterRole grants unexpected Kubernetes resource %q", resource)
			}
			for _, rawVerb := range verbs {
				verb, _ := rawVerb.(string)
				if verb != verbGet && verb != verbList && verb != "watch" {
					t.Fatalf("shared-gateway ClusterRole grants unexpected Kubernetes verb %q", verb)
				}
			}
		}
	}
}

// Ledgers are BudgetLedger resources beside the gateway (ADR 0045). A gateway
// that could update ConfigMaps in sproozi-system could rewrite its own egress,
// MCP, pricing and auth configuration, which take effect on its next restart,
// so its ledger Role names budgetledgers and nothing else.
func TestSharedGatewayHasNamespacedDurableBudgetRBAC(t *testing.T) {
	docs := renderInstall(t)
	foundRole := false
	foundNamespaceEnv := false
	for _, doc := range objectsOfKind(docs, "Namespace") {
		if metadataName(doc) == "sproozi-budget" {
			t.Fatal("install still creates the sproozi-budget namespace")
		}
	}
	gatewayRoles := map[string]bool{}
	for _, doc := range objectsOfKind(docs, kindRoleBinding) {
		subjects := fmt.Sprint(doc["subjects"])
		if metadataNamespace(doc) == systemNamespace && strings.Contains(subjects, gatewayAccount) {
			roleRef, _ := doc["roleRef"].(map[string]any)
			name, _ := roleRef["name"].(string)
			gatewayRoles[name] = true
		}
	}
	for _, doc := range objectsOfKind(docs, "Role") {
		ruleText := fmt.Sprint(doc["rules"])
		if metadataNamespace(doc) == systemNamespace && gatewayRoles[metadataName(doc)] &&
			strings.Contains(ruleText, resourceConfigMaps) &&
			(strings.Contains(ruleText, verbUpdate) || strings.Contains(ruleText, verbCreate)) {
			t.Fatalf("gateway Role %s can write ConfigMaps in %s", metadataName(doc), systemNamespace)
		}
		if metadataName(doc) != "sproozi-shared-gateway-budget" || metadataNamespace(doc) != budgetNamespace {
			continue
		}
		foundRole = true
		rules, _ := doc["rules"].([]any)
		var serialized strings.Builder
		for _, rule := range rules {
			fmt.Fprint(&serialized, rule)
		}
		for _, want := range []string{"budgetledgers", groupSproozi, verbGet, verbCreate, verbUpdate} {
			if !strings.Contains(serialized.String(), want) {
				t.Fatalf("budget Role is missing %q: %v", want, rules)
			}
		}
		if strings.Contains(serialized.String(), resourceConfigMaps) {
			t.Fatalf("budget Role grants ConfigMaps: %v", rules)
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
			foundNamespaceEnv = foundNamespaceEnv || (entry["name"] == "BUDGET_NAMESPACE" && entry["value"] == budgetNamespace)
		}
	}
	if !foundRole || !foundNamespaceEnv {
		t.Fatalf("shared gateway durable budget configuration missing (role=%v env=%v)", foundRole, foundNamespaceEnv)
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
	} {
		if !slices.Contains(names, want) {
			t.Fatalf("expected rendered network policy %q, got %v", want, names)
		}
	}
}

// Managed sessions span the gateway and the remote executor, and both default
// to the same budget namespace: moving one without the other broke managed
// sessions. The install binds no registry read: nothing runs managed sessions
// in sproozi-system. TestManagedSessionPathIsAuthorized checks, from resolved
// rules, that the gateway reads registrations in sproozi-sessions and cannot
// write them.
func TestGatewayAndExecutorShareManagedSessionState(t *testing.T) {
	for _, doc := range objectsOfKind(renderInstall(t), "Role") {
		if metadataNamespace(doc) == systemNamespace && strings.Contains(metadataName(doc), "registry") {
			t.Fatalf("install binds a registry Role in %s: %s", systemNamespace, metadataName(doc))
		}
	}
	if budgetNamespace != placement.DefaultBudgetNamespace {
		t.Fatalf("gateway ledger namespace %q differs from the executor default %q",
			budgetNamespace, placement.DefaultBudgetNamespace)
	}
}

// Sandbox isolation must not fail open: a Pod in sproozi-agents whose per-run
// policy is missing or no longer selects it still has no network.
func TestAgentNamespaceDefaultDenies(t *testing.T) {
	agentRBAC := renderKustomization(t, filepath.Join(repoRoot(t), "config", "agent-rbac"))
	for _, doc := range objectsOfKind(agentRBAC, "NetworkPolicy") {
		spec, _ := doc["spec"].(map[string]any)
		selector, _ := spec["podSelector"].(map[string]any)
		types := fmt.Sprint(spec["policyTypes"])
		if metadataNamespace(doc) == agentsNamespace && len(selector) == 0 &&
			strings.Contains(types, "Ingress") && strings.Contains(types, "Egress") &&
			spec["ingress"] == nil && spec["egress"] == nil {
			return
		}
	}
	t.Fatal("config/agent-rbac must install a default-deny NetworkPolicy for every Pod in sproozi-agents")
}
