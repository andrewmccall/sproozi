package model_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/audit"
	model "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

const refreshBody = `{"grant_type":"refresh_token","refresh_token":"sk-ant-ort01-harness-refresh","client_id":"claude-code"}`

func signInRequest(method, target, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Cookie", "browser-session")
	return r.WithContext(gateway.WithIdentity(r.Context(), testExecutionGrant()))
}

// Copilot sends its client identity and versions with the managed-settings
// request. The provider's policy, including a refusal, must reach it unchanged.
func TestCopilotAccountPolicyQueriesPreserveProviderDecision(t *testing.T) {
	const query = "client_id=github%2Fcli&client_version=1.0.103&copilot_runtime_version=1.0.103"
	const path = "/copilot_internal/managed_settings?" + query
	for _, policy := range []struct {
		name, body string
		status     int
	}{
		{"settings", `{"mcp":{"enabled":false}}`, http.StatusOK},
		{"refusal", `{"message":"Copilot is disabled by organization policy"}`, http.StatusForbidden},
	} {
		t.Run(policy.name, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || r.URL.RequestURI() != path ||
					r.Header.Get("Authorization") != "Bearer "+copilotGitHubToken {
					t.Errorf("policy request changed: %s %s", r.Method, r.URL.RequestURI())
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(policy.status)
				_, _ = io.WriteString(w, policy.body)
			}))
			defer upstream.Close()
			var logs bytes.Buffer
			provider, _ := model.ProviderFor(model.ProviderCopilot)
			h := provider.AccountRelay(modelauth.Workload{}, upstream.Client(), audit.NewLogger(&logs))
			h.UpstreamURL = upstream.URL
			r := signInRequest(http.MethodGet, "https://api.github.com"+path, "")
			r.Header.Set("Authorization", "Bearer "+copilotGitHubToken)
			response := httptest.NewRecorder()
			h.ServeHTTP(response, r)
			if response.Code != policy.status || response.Body.String() != policy.body || calls != 1 {
				t.Fatalf("provider policy not preserved: status=%d body=%s calls=%d", response.Code, response.Body.String(), calls)
			}
			for _, secret := range []string{copilotGitHubToken, query, policy.body} {
				if strings.Contains(logs.String(), secret) {
					t.Fatal("account relay audit included a credential, query or policy body")
				}
			}
		})
	}
}

func TestCopilotAccountRelayKeepsMethodPathAndQueryRestrictions(t *testing.T) {
	const registryPath = "/copilot/mcp_registry"
	const noQuery = "method GET; query parameters present: false"
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = io.WriteString(w, "provider settings")
	}))
	defer upstream.Close()
	var logs bytes.Buffer
	provider, _ := model.ProviderFor(model.ProviderCopilot)
	h := provider.AccountRelay(modelauth.Workload{}, upstream.Client(), audit.NewLogger(&logs))
	h.UpstreamURL = upstream.URL
	for _, path := range []string{
		"/copilot_internal/managed_settings?client_id=github%2Fcli",
		registryPath,
		"/copilot/future/account?client_version=fixture",
		"/copilot_internal/user?client_version=fixture",
	} {
		allowed := httptest.NewRecorder()
		h.ServeHTTP(allowed, signInRequest(http.MethodGet, "https://api.github.com"+path, ""))
		if allowed.Code != http.StatusOK || allowed.Body.String() != "provider settings" {
			t.Fatalf("allowed account request did not reach the provider: %s: %d %s", path, allowed.Code, allowed.Body.String())
		}
	}
	allowedCalls := calls
	for _, tc := range []struct{ method, path, diagnostic string }{
		{http.MethodPost, registryPath, "method POST; query parameters present: false"},
		{http.MethodDelete, registryPath, "method DELETE; query parameters present: false"},
		{http.MethodGet, "/copilotish/user", noQuery},
		{http.MethodGet, "/repos/owner/repo", noQuery},
		{http.MethodGet, "/copilot/../repos", noQuery},
		{http.MethodGet, "/copilot/%2e%2e/repos", noQuery},
		{http.MethodPost, "/copilot_internal/managed_settings?secret-name=secret-value", "method POST; query parameters present: true"},
		{"secret-method", "/copilot_internal/managed_settings", "method unrecognized; query parameters present: false"},
	} {
		logs.Reset()
		response := httptest.NewRecorder()
		h.ServeHTTP(response, signInRequest(tc.method, "https://api.github.com"+tc.path, ""))
		if response.Code != http.StatusForbidden || calls != allowedCalls ||
			!strings.Contains(response.Body.String(), tc.diagnostic) || !strings.Contains(logs.String(), tc.diagnostic) {
			t.Fatalf("restriction or diagnostic failed: %s %s: %d %s calls=%d audit=%s",
				tc.method, tc.path, response.Code, response.Body.String(), calls, logs.String())
		}
		for _, secret := range []string{"secret-name", "secret-value", "secret-method"} {
			if strings.Contains(logs.String()+response.Body.String(), secret) {
				t.Fatal("refusal exposed query content or an unrecognized method")
			}
		}
	}
}

func TestCopilotMCPRegistryPreservesProviderPolicy(t *testing.T) {
	for _, policy := range []struct {
		status int
		body   string
	}{
		{http.StatusOK, `{"mcp_registries":[],"registry_access":"allow_all"}`},
		{http.StatusForbidden, `{"message":"MCP servers are restricted by organization policy"}`},
		{http.StatusNotFound, `{"message":"No registry policy for this account"}`},
	} {
		calls := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.Method != http.MethodGet || r.URL.RequestURI() != "/copilot/mcp_registry" ||
				r.Header.Get("Authorization") != "Bearer "+copilotGitHubToken {
				t.Errorf("registry request changed: %s %s", r.Method, r.URL.RequestURI())
			}
			w.WriteHeader(policy.status)
			_, _ = io.WriteString(w, policy.body)
		}))
		provider, _ := model.ProviderFor(model.ProviderCopilot)
		relay := provider.AccountRelay(modelauth.Workload{}, upstream.Client(), nil)
		relay.UpstreamURL = upstream.URL
		request := signInRequest(http.MethodGet, "https://api.github.com/copilot/mcp_registry", "")
		request.Header.Set("Authorization", "Bearer "+copilotGitHubToken)
		response := httptest.NewRecorder()
		relay.ServeHTTP(response, request)
		upstream.Close()
		if response.Code != policy.status || response.Body.String() != policy.body || calls != 1 {
			t.Fatalf("registry policy changed: status=%d body=%s calls=%d", response.Code, response.Body.String(), calls)
		}
	}
}

func TestSignInRelaysTheHarnessRefreshUnchangedAndLogsNoTokens(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/oauth/token" || string(body) != refreshBody || r.Header.Get("Cookie") != "" {
			t.Errorf("relayed path=%s body=%s cookie=%q", r.URL.Path, body, r.Header.Get("Cookie"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"sk-ant-oat01-new","refresh_token":"sk-ant-ort01-new"}`)
	}))
	defer upstream.Close()
	var logs bytes.Buffer
	h := model.NewAnthropicSignInHandler(upstream.URL, upstream.Client(), audit.NewLogger(&logs))
	response := httptest.NewRecorder()
	h.ServeHTTP(response, signInRequest(http.MethodPost, "https://platform.claude.com/v1/oauth/token", refreshBody))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "sk-ant-oat01-new") ||
		response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(logs.String(), "sk-ant-") || !strings.Contains(logs.String(), `"allowed":true`) {
		t.Fatalf("audit leaked a token or missed the call: %s", logs.String())
	}
}

func TestSignInRefusesEverythingButTheTokenRequest(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer upstream.Close()
	h := model.NewAnthropicSignInHandler(upstream.URL, upstream.Client(), audit.NewLogger(io.Discard))
	for _, r := range []*http.Request{
		signInRequest(http.MethodGet, "https://platform.claude.com/v1/oauth/token", ""),
		signInRequest(http.MethodPost, "https://platform.claude.com/v1/organizations", "{}"),
		signInRequest(http.MethodPost, "https://platform.claude.com/v1/oauth/token?redirect=x", refreshBody),
		signInRequest(http.MethodPost, "https://platform.claude.com/v1/oauth/token", strings.Repeat("a", 65<<10)),
	} {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, r)
		if response.Code < 400 {
			t.Fatalf("%s %s was relayed: status=%d", r.Method, r.URL, response.Code)
		}
	}
	if calls != 0 {
		t.Fatalf("refused requests reached the provider %d times", calls)
	}
}
