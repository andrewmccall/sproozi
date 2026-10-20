package harness

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
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

// A Docker launch carries the profile in its configuration, so the configure
// bound must fit the largest archive beside large approved instructions.
func TestConfigureBoundFitsTheLargestProfile(t *testing.T) {
	launch := NativeLaunch{Harness: NativeCodex, Deadline: time.Now().Add(time.Hour), Env: []string{},
		Instructions: strings.Repeat("i", 64<<10), InstructionDigest: fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Repeat("i", 64<<10)))),
		RolloutMaxBytes: NativeRolloutMaxBytes, ClientConfig: &NativeClientConfig{MCPGateway: "https://gateway.test"},
		ProfileArchive: make([]byte, NativeProfileArchiveMaxBytes)}
	if err := launch.Validate(time.Now()); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(launch)
	if err != nil || len(encoded) > NativeConfigureMaxBytes {
		t.Fatalf("largest launch is %d bytes; bound %d", len(encoded), NativeConfigureMaxBytes)
	}
	launch.ProfileArchive = make([]byte, NativeProfileArchiveMaxBytes+1)
	if launch.Validate(time.Now()) == nil {
		t.Fatal("oversized profile archive accepted")
	}
}
