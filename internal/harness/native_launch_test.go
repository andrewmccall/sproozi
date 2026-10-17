package harness

import (
	"slices"
	"testing"
)

func TestNativeBaseEnvironmentSelectsOnlyTheApprovedHarnessPlaceholder(t *testing.T) {
	for _, tc := range []struct {
		kind       NativeKind
		key, other string
	}{
		{NativeCodex, nativeOpenAIKey, nativeAnthropicKey},
		{NativeClaude, nativeAnthropicKey, nativeOpenAIKey},
	} {
		env, err := NativeBaseEnvironment(tc.kind, "exact-incarnation")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(env, tc.key+"=sproozi-worker-placeholder") || !slices.Contains(env, "SPROOZI_EXECUTION_UID=exact-incarnation") || slices.Contains(env, tc.other+"=sproozi-worker-placeholder") {
			t.Fatalf("environment=%v", env)
		}
	}
	if _, err := NativeBaseEnvironment(NativeKind("unknown"), "exact-incarnation"); err == nil {
		t.Fatal("unknown harness admitted")
	}
}
