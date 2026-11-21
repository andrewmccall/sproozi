package main

import (
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// Every configuration key a Codex launch sets, through its options, the
// registry's key and sign-in options, its approved and control connections
// and its model upgrade notice, is one a user's -c override may not set, so a
// key the launch renders next cannot be left uncontrolled. Options an
// administrator adds to a runtime are not Sproozi's and are not checked.
func TestEveryKeyACodexLaunchSetsIsControlled(t *testing.T) {
	h, err := harness.Managed(harness.NativeCodex)
	if err != nil {
		t.Fatal(err)
	}
	notice, err := migrationNotice([]byte(`{"models":[{"slug":"gpt-5.5","upgrade":{"model":"gpt-6.1-sol"}}]}`))
	if err != nil || notice == "" {
		t.Fatalf("migration notice %q: %v", notice, err)
	}
	clients := &harness.NativeClientConfig{MCPGateway: "https://gateway.test", MCPServers: []string{"github", "docs"}}
	options := append(append([]string{notice}, h.KeyOptions...), h.SignInOptions...)
	args := codexArguments(config{Harness: harness.NativeCodex, Instructions: instructions, ClientConfig: clients,
		Options: options, Model: "configured-model"}, selected)
	keys := 0
	for i, arg := range args {
		if arg != "-c" || i+1 >= len(args) {
			continue
		}
		key, _, _ := strings.Cut(args[i+1], "=")
		keys++
		if err := harness.ValidateNativeArgs(harness.NativeCodex, []string{"-c", key + "=x"}); err == nil {
			t.Errorf("a Codex launch sets %q, but a user's -c override of it is accepted; add it to Codex's "+
				"ControlledKeys in internal/harness/native_args.go", key)
		}
	}
	if keys == 0 {
		t.Fatal("the rendered Codex launch sets no configuration key")
	}
}
