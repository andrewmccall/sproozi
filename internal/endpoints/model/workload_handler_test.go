package model_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	model "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

const (
	workloadOAuthToken = "sk-ant-oat01-harness-own-sign-in"
	workloadOpenAIKey  = "sk-proj-harness-own-key"
	completedStream    = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"total_tokens\":12,\"input_tokens\":9,\"output_tokens\":3}}}\n\n"
)

// workloadRoutes serves one model handler in workload mode behind a Dispatcher.
func workloadRoutes(authority string, handler http.Handler, tracker *budget.Tracker) gateway.Dispatcher {
	return gateway.Dispatcher{authority: {Capability: string(api.CapabilityModelInference), Handler: handler,
		Budgets: tracker, ProviderCredential: true}}
}

func TestWorkloadModeForwardsTheHarnessAnthropicSignIn(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+workloadOAuthToken || r.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" {
			t.Errorf("the harness's own sign-in did not reach the provider: authorization=%q beta=%q",
				r.Header.Get("Authorization"), r.Header.Get("Anthropic-Beta"))
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("cookie or gateway credential reached the provider")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicSuccess)
	}))
	defer upstream.Close()
	tracker := budget.NewTracker()
	h := model.NewAnthropicHandler(model.HandlerConfig{ProviderAuth: modelauth.Workload{}, UpstreamURL: upstream.URL,
		HTTPClient: upstream.Client(), AuditLogger: audit.NewLogger(io.Discard)})
	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages",
		strings.NewReader(`{"model":"claude-fixture","max_tokens":2,"messages":[{"role":"user","content":"hello"}]}`))
	req = req.WithContext(gateway.WithIdentity(req.Context(), testExecutionGrant()))
	req.Header.Set("Authorization", "Bearer "+workloadOAuthToken)
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	req.Header.Set("Cookie", "workload-session")
	response := httptest.NewRecorder()
	workloadRoutes(model.AnthropicAuthority, h, tracker).ServeHTTP(response, req)
	state := observedState(t, tracker, testRunUID+"/model.inference")
	if response.Code != http.StatusOK || state.UnitsUsed != 22 || state.ReservedUnits != 0 {
		t.Fatalf("status=%d body=%s state=%+v", response.Code, response.Body.String(), state)
	}
}

func TestWorkloadModeForwardsTheHarnessOpenAIKeyAndAccount(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+workloadOpenAIKey || r.Header.Get("OpenAI-Project") != "proj-own" {
			t.Errorf("the harness's own key and project did not reach the provider: authorization=%q project=%q",
				r.Header.Get("Authorization"), r.Header.Get("OpenAI-Project"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completedStream)
	}))
	defer upstream.Close()
	tracker := budget.NewTracker()
	h := &model.Handler{Config: model.HandlerConfig{ProviderAuth: modelauth.Workload{}, UpstreamURL: upstream.URL,
		HTTPClient: upstream.Client(), AuditLogger: audit.NewLogger(io.Discard)}}
	req := httptest.NewRequest(http.MethodPost, "https://api.openai.com"+testResponsesPath,
		strings.NewReader(`{"model":"test","stream":true}`))
	req = req.WithContext(gateway.WithIdentity(req.Context(), testExecutionGrant()))
	req.Header.Set("Authorization", "Bearer "+workloadOpenAIKey)
	req.Header.Set("OpenAI-Project", "proj-own")
	response := httptest.NewRecorder()
	workloadRoutes(model.OpenAIAuthority, h, tracker).ServeHTTP(response, req)
	state := observedState(t, tracker, testRunUID+"/model.inference")
	if response.Code != http.StatusOK || response.Body.String() != completedStream || state.UnitsUsed != 12 {
		t.Fatalf("status=%d body=%s state=%+v", response.Code, response.Body.String(), state)
	}
}

func TestWorkloadModeRefusesAHarnessThatIsNotSignedIn(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	tracker := budget.NewTracker()
	h := model.NewAnthropicHandler(model.HandlerConfig{ProviderAuth: modelauth.Workload{}, UpstreamURL: upstream.URL,
		HTTPClient: upstream.Client(), AuditLogger: audit.NewLogger(io.Discard)})
	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages",
		strings.NewReader(`{"model":"claude-fixture","max_tokens":2,"messages":[{"role":"user","content":"hello"}]}`))
	req = req.WithContext(gateway.WithIdentity(req.Context(), testExecutionGrant()))
	response := httptest.NewRecorder()
	workloadRoutes(model.AnthropicAuthority, h, tracker).ServeHTTP(response, req)
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "sign the harness in") ||
		calls.Load() != 0 {
		t.Fatalf("status=%d body=%s upstream calls=%d", response.Code, response.Body.String(), calls.Load())
	}
	if state, err := tracker.State(testRunUID + "/model.inference"); err == nil && (state.UnitsUsed != 0 || state.ReservedUnits != 0) {
		t.Fatalf("a harness that isn't signed in reserved budget: %+v", state)
	}
}
