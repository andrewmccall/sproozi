package manifests_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// The shipped managed Codex runtime must name a model provider that reads its
// key from the environment. Without one, the stock Codex terminal waits at its
// sign-in screen and never starts the task, while a test that adds the options
// itself still passes.
func TestManagedCodexExampleReadsItsKeyFromTheEnvironment(t *testing.T) {
	docs := decodeFile(t, filepath.Join(repoRoot(t), "examples", "portable-workflows", "kubernetes", "runtime.yaml"))
	runtimes := objectsOfKind(docs, "AgentRuntime")
	if len(runtimes) != 1 {
		t.Fatalf("want one AgentRuntime, got %d", len(runtimes))
	}
	spec, _ := runtimes[0]["spec"].(map[string]any)
	client, _ := spec["clientConfig"].(map[string]any)
	native, _ := client["nativeSession"].(map[string]any)
	options, _ := native["options"].([]any)
	var provider string
	keys := map[string]bool{}
	for _, raw := range options {
		option := fmt.Sprint(raw)
		if value, ok := strings.CutPrefix(option, "model_provider="); ok {
			provider = strings.Trim(value, `"`)
		}
		keys[option] = true
	}
	if provider == "" {
		t.Fatalf("runtime.yaml sets no model_provider: %v", options)
	}
	for _, want := range []string{
		"model_providers." + provider + `.env_key="OPENAI_API_KEY"`,
		"model_providers." + provider + `.wire_api="responses"`,
	} {
		if !keys[want] {
			t.Errorf("runtime.yaml lacks %s", want)
		}
	}
}
