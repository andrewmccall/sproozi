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
	placeholder := func(key string) string { return key + "=sproozi-worker-placeholder" }
	for _, tc := range []struct {
		kind       NativeKind
		credential NativeCredential
		want       []string
		absent     []string
	}{
		{NativeCodex, CredentialKey, []string{placeholder(nativeOpenAIKey), placeholder("GH_TOKEN")}, []string{placeholder(nativeAnthropicKey)}},
		{NativeCodex, CredentialToken, []string{placeholder(nativeOpenAIKey)}, nil},
		{NativeClaude, CredentialKey, []string{placeholder(nativeAnthropicKey)}, []string{placeholder(nativeOpenAIKey)}},
		{NativeClaude, CredentialToken, []string{placeholder(nativeAnthropicKey)}, nil},
		{NativeCopilot, CredentialKey, []string{placeholder("COPILOT_PROVIDER_API_KEY"), placeholder("GH_TOKEN")},
			[]string{"COPILOT_GITHUB_TOKEN=" + NativeCopilotTokenPlaceholder}},
		// Copilot calls its plan's service with a GitHub token the gateway replaces.
		{NativeCopilot, CredentialToken, []string{"COPILOT_GITHUB_TOKEN=" + NativeCopilotTokenPlaceholder},
			[]string{placeholder("COPILOT_PROVIDER_API_KEY")}},
		// Copilot prefers GH_TOKEN to its own /login, so it gets none.
		{NativeCopilot, CredentialSignIn, nil, []string{placeholder("GH_TOKEN"), placeholder("COPILOT_PROVIDER_API_KEY")}},
	} {
		env, err := NativeBaseEnvironment(tc.kind, "exact-incarnation", tc.credential)
		if err != nil || !slices.Contains(env, "SPROOZI_EXECUTION_UID=exact-incarnation") {
			t.Fatalf("%s %s: %v %v", tc.kind, tc.credential, env, err)
		}
		for _, entry := range tc.want {
			if !slices.Contains(env, entry) {
				t.Fatalf("%s %s: missing %s in %v", tc.kind, tc.credential, entry, env)
			}
		}
		for _, entry := range tc.absent {
			if slices.Contains(env, entry) {
				t.Fatalf("%s %s: unexpected %s", tc.kind, tc.credential, entry)
			}
		}
	}
	for _, kind := range ManagedKinds() {
		signedIn, err := NativeBaseEnvironment(kind, "exact-incarnation", CredentialSignIn)
		if err != nil || slices.ContainsFunc(signedIn, func(e string) bool { return strings.HasSuffix(e, "_API_KEY=sproozi-worker-placeholder") }) {
			t.Fatalf("%s signs in itself but got a placeholder key: %v %v", kind, signedIn, err)
		}
	}
	if _, err := NativeBaseEnvironment(NativeKind("unknown"), "exact-incarnation", CredentialKey); err == nil {
		t.Fatal("unknown harness admitted")
	}
	if _, err := NativeBaseEnvironment(NativeCodex, "exact-incarnation", "keyring"); err == nil {
		t.Fatal("unknown credential admitted")
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
