package manifests_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The Claude Code executor example: a second remote executor, in its own
// namespace, beside the Codex one. One executor runs one harness, so Codex
// and Claude Code on one cluster are two executors that the CLI selects with
// --namespace (ADR 0057).
const (
	claudeSessionsNamespace = "sproozi-claude-sessions"
	claudeExample           = "claude"
)

func claudeExampleDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "portable-workflows", "kubernetes", claudeExample)
}

// claudeExecutorProfile is the executor configuration the example documents.
type claudeExecutorProfile struct {
	Harness           string `json:"harness"`
	PlacementName     string `json:"placementName"`
	Namespace         string `json:"namespace"`
	TemplateName      string `json:"templateName"`
	InstructionDigest string `json:"instructionDigest"`
	WorkTokenFile     string `json:"workTokenFile"`
}

// The example's runtime, policy, template and executor configuration agree:
// one managed claude-code template whose runtime signs in through a login
// claim as the release image's UID and GID 1000, and an executor configured for
// that harness, namespace, template and the template's exact instructions.
func TestClaudeExecutorExampleIsConsistent(t *testing.T) {
	docs := renderKustomization(t, claudeExampleDir(t))
	for _, doc := range docs {
		if ns := metadataNamespace(doc); doc[keyKind] != "Namespace" && ns != "" &&
			ns != claudeSessionsNamespace && ns != agentsNamespace && ns != systemNamespace {
			t.Errorf("%s %s is in namespace %s", doc[keyKind], metadataName(doc), ns)
		}
	}
	runtimes := objectsOfKind(docs, "AgentRuntime")
	templates := objectsOfKind(docs, "AgentTemplate")
	policies := objectsOfKind(docs, "AgentPolicy")
	if len(runtimes) != 1 || len(templates) != 1 || len(policies) != 1 {
		t.Fatalf("want one runtime, template and policy: %d %d %d", len(runtimes), len(templates), len(policies))
	}
	runtime, template, policy := runtimes[0], templates[0], policies[0]
	for _, doc := range []map[string]any{runtime, template, policy} {
		if metadataNamespace(doc) != claudeSessionsNamespace {
			t.Errorf("%s %s is not in %s", doc[keyKind], metadataName(doc), claudeSessionsNamespace)
		}
	}
	spec := field(runtime, "spec")
	client := field(spec, "clientConfig")
	if client["harness"] != "claude-code" {
		t.Errorf("runtime harness %v", client["harness"])
	}
	if _, ok := field(client, "signIn")["claims"].(map[string]any); !ok {
		t.Error("runtime does not keep Claude Code's login in a claim (signIn.claims)")
	}
	if _, ok := field(client, "nativeSession")["options"]; ok {
		t.Error("Claude Code takes no native options")
	}
	pod := field(field(spec, "podTemplate"), "spec")
	security := field(pod, "securityContext")
	for _, key := range []string{"runAsUser", "runAsGroup", "fsGroup"} {
		if fmt.Sprint(security[key]) != "1000" {
			t.Errorf("runtime %s is %v, not the native image's 1000", key, security[key])
		}
	}

	templateSpec := field(template, "spec")
	annotations := field(field(template, "metadata"), "annotations")
	if annotations["sessions.sproozi.com/managed"] != "true" {
		t.Error("template is not a managed-session template")
	}
	if fmt.Sprint(field(templateSpec, "runtimeRef")[keyName]) != metadataName(runtime) ||
		fmt.Sprint(field(templateSpec, "policyRef")[keyName]) != metadataName(policy) {
		t.Errorf("template references %v and %v", templateSpec["runtimeRef"], templateSpec["policyRef"])
	}
	instructions, _ := templateSpec["instructions"].(string)

	data, err := os.ReadFile(filepath.Join(claudeExampleDir(t), "profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	var profile claudeExecutorProfile
	if err = json.Unmarshal(data, &profile); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(instructions))
	switch {
	case profile.Harness != "claude-code":
		t.Errorf("executor harness %q", profile.Harness)
	case profile.Namespace != claudeSessionsNamespace:
		t.Errorf("executor namespace %q", profile.Namespace)
	case profile.TemplateName != metadataName(template):
		t.Errorf("executor template %q, example template %q", profile.TemplateName, metadataName(template))
	case profile.InstructionDigest != hex.EncodeToString(sum[:]):
		t.Errorf("executor instructionDigest %q is not the template's instructions' SHA-256", profile.InstructionDigest)
	case profile.PlacementName == "" || profile.PlacementName == "local":
		t.Errorf("executor placementName %q", profile.PlacementName)
	case profile.WorkTokenFile == "":
		t.Error("the executor's owner API needs workTokenFile")
	}
}

// Both executors apply to one cluster: none of the Claude example's objects
// replaces one of the Codex example's, or of its CLI access.
func TestClaudeExecutorExampleAppliesBesideTheCodexOne(t *testing.T) {
	root := repoRoot(t)
	key := func(doc map[string]any) string {
		return fmt.Sprintf("%v/%s/%s", doc[keyKind], metadataNamespace(doc), metadataName(doc))
	}
	codex := map[string]bool{}
	for _, path := range []string{
		filepath.Join(root, "examples", "portable-workflows", "kubernetes"),
		filepath.Join(root, "config", "cli-access"),
	} {
		for _, doc := range renderKustomization(t, path) {
			codex[key(doc)] = true
		}
	}
	for _, path := range []string{claudeExampleDir(t), filepath.Join(claudeExampleDir(t), "cli-access")} {
		for _, doc := range renderKustomization(t, path) {
			if codex[key(doc)] {
				t.Errorf("%s replaces the Codex example's %s", path, key(doc))
			}
		}
	}
}

func field(doc map[string]any, name string) map[string]any {
	value, _ := doc[name].(map[string]any)
	return value
}
