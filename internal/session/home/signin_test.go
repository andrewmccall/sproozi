package home

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

func TestCodexWithoutAProviderKeySignsInItself(t *testing.T) {
	c := composedConfig(t)
	c.Gateway.ProviderTokenFile = ""
	if problems := c.ServeProblems(); slices.ContainsFunc(problems, func(p string) bool {
		return strings.HasPrefix(p, "gateway.providerTokenFile")
	}) {
		t.Fatalf("an omitted key file was reported for Codex: %v", problems)
	}
	modelConfig, err := provider(c.Gateway, harness.NativeCodex)
	if err != nil || modelConfig.ProviderAuth.Mode() != modelauth.WorkloadMode {
		t.Fatalf("Codex's gateway doesn't forward its sign-in: %v", err)
	}
	stacks, err := localConfigs(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"api.openai.com", "chatgpt.com", "auth.openai.com"} {
		if !slices.Contains(servingNames(c, stacks).DNS, host) {
			t.Fatalf("the certificate doesn't cover %s", host)
		}
	}
	keyed, err := localConfigs(composedConfig(t))
	if err != nil || keyed[0].config.SignIn || slices.Contains(servingNames(composedConfig(t), keyed).DNS, "chatgpt.com") {
		t.Fatalf("a configured key still signed in or covered the subscription host: %v", err)
	}
}

// A sign-in token held by the gateway is attached as a bearer with the
// provider's own token headers, for any harness of that provider.
func TestGatewayHeldSignInTokenUsesTheProvidersHeaders(t *testing.T) {
	c := composedConfig(t)
	c.Local.Harness, c.Gateway.ProviderTokenKind = harness.NativeClaude, modelauth.TokenMode
	modelConfig, err := provider(c.Gateway, harness.NativeClaude)
	if err != nil {
		t.Fatal(err)
	}
	token, ok := modelConfig.ProviderAuth.(modelauth.Token)
	if !ok || token.Headers["Anthropic-Beta"] != "oauth-2025-04-20" {
		t.Fatalf("auth = %#v", modelConfig.ProviderAuth)
	}
	c.Gateway.ProviderTokenKind = "oauth"
	if !slices.ContainsFunc(c.ServeProblems(), func(p string) bool { return strings.HasPrefix(p, "gateway.providerTokenKind") }) {
		t.Fatal("an unknown token kind was accepted")
	}
}

func TestGatewayHeldChatGPTSignInRefreshesFromItsFile(t *testing.T) {
	c := composedConfig(t)
	session := modelauth.Session{Issuer: modelauth.Issuer, Subject: "account", ClientID: "oaiapp_fixture",
		HostID: "urn:uuid:fixture", AccessToken: "fixture-access", RefreshToken: "fixture-refresh", IDToken: "fixture-id",
		TokenType: "Bearer", ExpiresIn: 3600, SavedAt: time.Now().UTC(), Scopes: []string{modelauth.PlanScope}}
	c.Gateway.ProviderTokenFile = filepath.Join(filepath.Dir(c.Gateway.ProviderTokenFile), "chatgpt-session.json")
	c.Gateway.ProviderTokenKind = modelauth.ChatGPTMode
	if err := modelauth.NewFileSessionStore(c.Gateway.ProviderTokenFile).Save(session); err != nil {
		t.Fatal(err)
	}
	modelConfig, err := provider(c.Gateway, harness.NativeCodex)
	if err != nil || modelConfig.ProviderAuth.Mode() != modelauth.ChatGPTMode {
		t.Fatalf("gateway auth: %v", err)
	}
	if slices.ContainsFunc(c.ServeProblems(), func(p string) bool { return strings.HasPrefix(p, "gateway.providerToken") }) {
		t.Fatalf("problems: %v", c.ServeProblems())
	}
}
