package docker

import (
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
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
		paths map[string][]string
	}{
		{"key", modelauth.APIKey{Key: "sk-fixture"}, []string{modelgateway.OpenAIAuthority}, nil},
		{"token", modelauth.Token{Value: "github_pat_fixture"}, append(slices.Clone(plans), "api.github.com:443"),
			map[string][]string{"api.github.com:443": {"/copilot_internal/", "/copilot/"}}},
		{"sign-in", modelauth.Workload{}, append(slices.Clone(plans), "api.github.com:443", "github.com:443"),
			map[string][]string{"api.github.com:443": {"/copilot_internal/", "/copilot/"}, "github.com:443": {"/login/"}}},
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
		for authority, prefixes := range tc.paths {
			route := dispatch[authority]
			if route.Handler != nil || len(route.Paths) != len(prefixes) {
				t.Fatalf("%s: %s has unexpected account routes: %+v", tc.name, authority, route)
			}
			for i, prefix := range prefixes {
				if route.Paths[i].Prefix != prefix {
					t.Fatalf("%s: %s path prefix %q, want %q", tc.name, authority, route.Paths[i].Prefix, prefix)
				}
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

func TestCopilotRegistryRequestTraversesDockerDispatcher(t *testing.T) {
	const policy = `{"mcp_registries":[],"registry_access":"allow_all"}`
	for _, auth := range []modelauth.Authenticator{modelauth.Workload{}, modelauth.Token{Value: "gho_gateway"}} {
		calls := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			wantCredential := "Bearer gho_gateway"
			if auth.Mode() == modelauth.WorkloadMode {
				wantCredential = "Bearer gho_client"
			}
			if r.Method != http.MethodGet || r.URL.RequestURI() != "/copilot/mcp_registry" ||
				r.Header.Get("Authorization") != wantCredential {
				t.Errorf("registry request changed: %s %s", r.Method, r.URL.RequestURI())
			}
			_, _ = io.WriteString(w, policy)
		}))
		dispatch, err := modelRoutes(GatewayConfig{Harness: harness.NativeCopilot},
			modelgateway.HandlerConfig{UpstreamURL: "https://api.openai.com", ProviderAuth: auth, HTTPClient: upstream.Client()}, nil)
		if err != nil {
			t.Fatal(err)
		}
		route, _, _ := dispatch.Select("api.github.com:443", "/copilot/mcp_registry")
		relay, ok := route.Handler.(*modelgateway.Relay)
		if !ok {
			t.Fatalf("registry request did not select the Copilot account relay: %+v", route)
		}
		relay.UpstreamURL = upstream.URL
		request := httptest.NewRequest(http.MethodGet, "https://api.github.com/copilot/mcp_registry", nil)
		request.Header.Set("Authorization", "Bearer gho_client")
		response := httptest.NewRecorder()
		route.Handler.ServeHTTP(response, request)
		upstream.Close()
		if response.Code != http.StatusOK || response.Body.String() != policy || calls != 1 {
			t.Fatalf("registry policy not relayed: status=%d body=%s calls=%d", response.Code, response.Body.String(), calls)
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
