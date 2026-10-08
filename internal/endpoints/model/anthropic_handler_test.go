package model_test

import (
	"encoding/json"
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

const anthropicSuccess = `{"type":"message","role":"assistant","id":"msg-fixture","model":"claude-fixture","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":2}}`

const testAnthropicSecret = "trusted-secret"

func TestAnthropicCappedNullableUsageSettlesActualConsumption(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request["service_tier"] != "standard_only" {
			t.Errorf("capped request did not pin standard pricing: %v %v", request, err)
		}
		_, _ = io.WriteString(w, strings.Replace(anthropicSuccess, `"output_tokens":2`, `"output_tokens":2,"cache_read_input_tokens":null,"cache_creation_input_tokens":null,"cache_creation":null`, 1))
	}))
	defer upstream.Close()
	identity := testRunIdentity()
	identity.Policy.Spec.Budgets[api.CapabilityModelInference] = api.EndpointBudget{MaxUnits: 100, MaxCostMicros: 200}
	tracker := budget.NewTracker()
	h := model.NewAnthropicHandler(model.HandlerConfig{
		Pricing:      &model.PricingTable{Models: map[string]model.ModelPrice{"claude-fixture": {InputMicrosPerMillionTokens: 1000000, CachedInputMicrosPerMillionTokens: 1000000, OutputMicrosPerMillionTokens: 1000000}}},
		ProviderAuth: modelauth.AnthropicAPIKey{Key: testAnthropicSecret}, UpstreamURL: upstream.URL,
		HTTPClient: upstream.Client(), AuditLogger: audit.NewLogger(io.Discard),
	})
	routes := gateway.Dispatcher{model.AnthropicAuthority: {Capability: string(api.CapabilityModelInference), Handler: h, Budgets: tracker}}
	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(`{"model":"claude-fixture","max_tokens":2,"cache_control":null,"messages":[{"role":"user","content":"hello"}]}`))
	req = req.WithContext(gateway.WithIdentity(req.Context(), identity))
	response := httptest.NewRecorder()
	routes.ServeHTTP(response, req)
	state := tracker.State(testRunUID + "/model.inference")
	if response.Code != 200 || state.UnitsUsed != 22 || state.CostMicros != 22 || state.ReservedUnits != 0 || state.ReservedCostMicros != 0 {
		t.Fatalf("status=%d body=%s state=%+v", response.Code, response.Body.String(), state)
	}
}

func TestAnthropicHandlerReplacesCredentialsAndSettlesTotalBeyondOutputBound(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != testAnthropicSecret || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Anthropic-Organization") != "" {
			t.Error("untrusted provider headers escaped")
		}
		if r.Header.Get("Anthropic-Version") != "2023-06-01" {
			t.Error("unsupported protocol version")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicSuccess)
	}))
	defer upstream.Close()
	identity := testRunIdentity()
	tracker := budget.NewTracker()
	h := model.NewAnthropicHandler(model.HandlerConfig{ProviderAuth: modelauth.AnthropicAPIKey{Key: testAnthropicSecret}, UpstreamURL: upstream.URL, HTTPClient: upstream.Client(), AuditLogger: audit.NewLogger(io.Discard)})
	routes := gateway.Dispatcher{model.AnthropicAuthority: {Capability: string(api.CapabilityModelInference), Handler: h, Budgets: tracker}}
	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(`{"model":"claude-fixture","max_tokens":2,"messages":[{"role":"user","content":"hello"}]}`))
	req = req.WithContext(gateway.WithIdentity(req.Context(), identity))
	req.Header.Set("x-api-key", "workload-key")
	req.Header.Set("Cookie", "workload-session")
	req.Header.Set("Anthropic-Organization", "workload-account")
	req.Header.Set("Authorization", "Bearer workload-token")
	response := httptest.NewRecorder()
	routes.ServeHTTP(response, req)
	state := tracker.State(testRunUID + "/model.inference")
	if response.Code != 200 || response.Body.String() != anthropicSuccess || state.UnitsUsed != 22 || state.ReservedUnits != 0 {
		t.Fatalf("status=%d body=%s state=%+v", response.Code, response.Body.String(), state)
	}
}

func TestAnthropicFailureConsumesBothCapsAndMixedProviderBudget(t *testing.T) {
	identity := testRunIdentity()
	identity.Policy.Spec.Budgets[api.CapabilityModelInference] = api.EndpointBudget{MaxUnits: 100, MaxCostMicros: 200}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"partial output"}]}`)
	}))
	defer upstream.Close()
	tracker := budget.NewTracker()
	config := model.HandlerConfig{Pricing: &model.PricingTable{Models: map[string]model.ModelPrice{"claude-fixture": {InputMicrosPerMillionTokens: 1, CachedInputMicrosPerMillionTokens: 1, OutputMicrosPerMillionTokens: 1}}}, ProviderAuth: modelauth.AnthropicAPIKey{Key: testAnthropicSecret}, UpstreamURL: upstream.URL, HTTPClient: upstream.Client(), AuditLogger: audit.NewLogger(io.Discard)}
	h := model.NewAnthropicHandler(config)
	openAI := &model.Handler{Config: config}
	routes := gateway.Dispatcher{
		model.AnthropicAuthority: {Capability: "model.inference", Handler: h, Budgets: tracker},
		"api.openai.com:443":     {Capability: "model.inference", Handler: openAI, Budgets: tracker},
	}
	for i, address := range []string{"https://api.anthropic.com/v1/messages", "https://api.openai.com/v1/responses"} {
		req := httptest.NewRequest(http.MethodPost, address, strings.NewReader(`{"model":"claude-fixture","max_tokens":2,"messages":[]}`))
		req = req.WithContext(gateway.WithIdentity(req.Context(), identity))
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, req)
		expected := 502
		if i == 1 {
			expected = 429
		}
		if response.Code != expected || strings.Contains(response.Body.String(), "partial output") {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	state := tracker.State(testRunUID + "/model.inference")
	if calls.Load() != 1 || state.UnitsUsed != 100 || state.CostMicros != 200 || state.ReservedUnits != 0 || state.ReservedCostMicros != 0 {
		t.Fatalf("calls=%d state=%+v", calls.Load(), state)
	}
}

func TestAnthropicDeniesUnpricedModelBeforeInference(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	identity := testRunIdentity()
	identity.Policy.Spec.Budgets[api.CapabilityModelInference] = api.EndpointBudget{MaxUnits: 100, MaxCostMicros: 200}
	h := model.NewAnthropicHandler(model.HandlerConfig{ProviderAuth: modelauth.AnthropicAPIKey{Key: testAnthropicSecret}, UpstreamURL: upstream.URL, HTTPClient: upstream.Client(), AuditLogger: audit.NewLogger(io.Discard)})
	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(`{"model":"unpriced","max_tokens":2,"messages":[]}`))
	req = req.WithContext(gateway.WithIdentity(req.Context(), identity))
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	if response.Code != 403 || calls.Load() != 0 {
		t.Fatalf("status=%d calls=%d", response.Code, calls.Load())
	}
}
