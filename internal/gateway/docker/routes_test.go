package docker

import (
	"io"
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/andrewmccall/sproozi/internal/audit"
	modelgateway "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

// Copilot's routes follow its mechanism (ADR 0048): a key serves OpenAI in
// bring-your-own-key mode; a gateway-held GitHub token serves its plan's
// hosts and account requests; its own sign-in also relays GitHub's device
// flow, as a path route beside the GitHub capability's Git route.
func TestCopilotModelRoutesFollowItsMechanism(t *testing.T) {
	plans := []string{modelgateway.CopilotIndividualAuthority, modelgateway.CopilotBusinessAuthority,
		modelgateway.CopilotEnterpriseAuthority}
	for _, tc := range []struct {
		name  string
		auth  modelauth.Authenticator
		hosts []string
		paths map[string]string
	}{
		{"key", modelauth.APIKey{Key: "sk-fixture"}, []string{modelgateway.OpenAIAuthority}, nil},
		{"token", modelauth.Token{Value: "github_pat_fixture"}, append(slices.Clone(plans), "api.github.com:443"),
			map[string]string{"api.github.com:443": "/copilot_internal/"}},
		{"sign-in", modelauth.Workload{}, append(slices.Clone(plans), "api.github.com:443", "github.com:443"),
			map[string]string{"api.github.com:443": "/copilot_internal/", "github.com:443": "/login/"}},
	} {
		config := GatewayConfig{Harness: harness.NativeCopilot, AuditLogger: audit.NewLogger(io.Discard)}
		model := modelgateway.HandlerConfig{UpstreamURL: "https://api.openai.com", ProviderAuth: tc.auth,
			HTTPClient: http.DefaultClient}
		dispatch, err := modelRoutes(config, model, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := slices.Sorted(maps.Keys(dispatch)); !slices.Equal(got, slices.Sorted(slices.Values(tc.hosts))) {
			t.Fatalf("%s: authorities %v, want %v", tc.name, got, tc.hosts)
		}
		for authority, prefix := range tc.paths {
			route := dispatch[authority]
			if route.Handler != nil || len(route.Paths) != 1 || route.Paths[0].Prefix != prefix {
				t.Fatalf("%s: %s is not only the path route %s: %+v", tc.name, authority, prefix, route)
			}
		}
		forwards := tc.name == "sign-in"
		for _, plan := range plans {
			if route, ok := dispatch[plan]; ok && route.ProviderCredential != forwards {
				t.Fatalf("%s: %s forwards the harness's credential: %v", tc.name, plan, route.ProviderCredential)
			}
		}
	}
}

// Codex and Claude Code keep their routes: a key or token serves the API
// host only; their own sign-in adds the signed-in hosts and the relay.
func TestOtherHarnessesKeepTheirModelRoutes(t *testing.T) {
	for _, tc := range []struct {
		kind  harness.NativeKind
		auth  modelauth.Authenticator
		hosts []string
	}{
		{harness.NativeCodex, modelauth.Token{Value: "fixture"}, []string{modelgateway.OpenAIAuthority}},
		{harness.NativeCodex, modelauth.Workload{}, []string{modelgateway.OpenAIAuthority,
			modelgateway.ChatGPTBackendAuthority, modelgateway.OpenAISignInAuthority}},
		{harness.NativeClaude, modelauth.AnthropicAPIKey{Key: "fixture"}, []string{modelgateway.AnthropicAuthority}},
	} {
		dispatch, err := modelRoutes(GatewayConfig{Harness: tc.kind, AuditLogger: audit.NewLogger(io.Discard)},
			modelgateway.HandlerConfig{UpstreamURL: "https://example.invalid", ProviderAuth: tc.auth, HTTPClient: http.DefaultClient}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := slices.Sorted(maps.Keys(dispatch)); !slices.Equal(got, slices.Sorted(slices.Values(tc.hosts))) {
			t.Fatalf("%s %s: authorities %v, want %v", tc.kind, tc.auth.Mode(), got, tc.hosts)
		}
	}
}
