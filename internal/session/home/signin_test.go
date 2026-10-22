package home

import (
	"slices"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

// A harness configured without a provider key signs in itself: its placement
// mounts its sign-in, its gateway forwards that credential, and the gateway
// certificate covers its sign-in host (ADR 0047).
func TestClaudeWithoutAProviderKeySignsInItself(t *testing.T) {
	c := composedConfig(t)
	c.Local.Harness, c.Gateway.ProviderTokenFile = harness.NativeClaude, ""
	if problems := c.ServeProblems(); slices.ContainsFunc(problems, func(p string) bool {
		return strings.HasPrefix(p, "gateway.providerTokenFile")
	}) {
		t.Fatalf("an omitted key file was reported for Claude: %v", problems)
	}
	if c.ProviderTokenProblem() != "" {
		t.Fatal("an omitted key file was reported as a placeholder")
	}
	stacks, err := localConfigs(c)
	if err != nil {
		t.Fatal(err)
	}
	if !stacks[0].config.SignIn {
		t.Fatal("the placement doesn't mount Claude's own sign-in")
	}
	if names := servingNames(c, stacks); !slices.Contains(names.DNS, "platform.claude.com") {
		t.Fatalf("the certificate doesn't cover Claude's sign-in host: %v", names.DNS)
	}
	modelConfig, err := provider(c.Gateway, harness.NativeClaude)
	if err != nil || modelConfig.ProviderAuth.Mode() != modelauth.WorkloadMode {
		t.Fatalf("Claude's gateway doesn't forward its sign-in: %v", err)
	}
}

func TestCodexStillNeedsAProviderKey(t *testing.T) {
	c := composedConfig(t)
	c.Gateway.ProviderTokenFile = ""
	if want := "gateway.providerTokenFile (absolute path; codex can't sign in itself yet)"; !slices.Contains(c.ServeProblems(), want) {
		t.Fatalf("missing %q in %v", want, c.ServeProblems())
	}
	if _, err := provider(c.Gateway, harness.NativeCodex); err == nil || !strings.Contains(err.Error(), "can't sign in itself") {
		t.Fatalf("codex without a key: %v", err)
	}
	stacks, err := localConfigs(composedConfig(t))
	if err != nil || stacks[0].config.SignIn {
		t.Fatalf("a configured key still mounted a sign-in: %v", err)
	}
}
