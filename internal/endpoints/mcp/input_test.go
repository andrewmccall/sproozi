package mcp

import "testing"

func TestRemoteToolRequiresObjectArguments(t *testing.T) {
	for _, kind := range []string{"array", "string", ""} {
		if _, _, err := compileInput(map[string]any{"type": kind}); err == nil {
			t.Fatalf("non-object remote tool schema accepted: %q", kind)
		}
	}
}
