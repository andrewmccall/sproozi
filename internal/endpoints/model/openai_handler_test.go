package model_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	modelgateway "github.com/andrewmccall/sproozi/internal/endpoints/model"
)

const testChatCompletionsPath = "/v1/chat/completions"

func TestOpenAIOperationsUseFixedMethodsWithoutQuery(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/v1/models", 200},
		{http.MethodPost, "/v1/responses", 200},
		{http.MethodPost, testChatCompletionsPath, 200},
		{http.MethodPost, "/v1/models", 403},
		{http.MethodGet, "/v1/responses", 403},
		{http.MethodPut, testChatCompletionsPath, 403},
		{http.MethodPost, "/v1/responses?mode=other", 403},
		{http.MethodGet, "/v1/models?", 403},
		{http.MethodPost, "/v1/%72esponses", 403},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			h := newHandler(t, testExecutionGrant(), testModelUpstream)
			calls := 0
			h.Config.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return modelResponse(r, `{"usage":{"total_tokens":3,"input_tokens":2,"output_tokens":1,"prompt_tokens":2,"completion_tokens":1}}`), nil
			})}
			var body io.Reader
			if tc.method == http.MethodPost {
				body = strings.NewReader(`{"model":"fixture-model"}`)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, body))
			wantCalls := 0
			if tc.want == 200 {
				wantCalls = 1
			}
			if w.Code != tc.want || calls != wantCalls {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
			}
		})
	}
}

func TestOpenAICostAdmissionChecksPriceBeforeProvider(t *testing.T) {
	price := modelgateway.ModelPrice{InputMicrosPerMillionTokens: 1_000_000, CachedInputMicrosPerMillionTokens: 1_000_000, OutputMicrosPerMillionTokens: 1_000_000}
	priced := &modelgateway.PricingTable{Models: map[string]modelgateway.ModelPrice{"fixture-model": price}}
	for _, tc := range []struct {
		name, body string
		pricing    *modelgateway.PricingTable
		want       int
	}{
		{"missing table", `{"model":"fixture-model"}`, nil, 403},
		{"missing model price", `{"model":"unlisted"}`, priced, 403},
		{"missing model", `{"input":[]}`, priced, 403},
		{"invalid price", `{"model":"fixture-model"}`, &modelgateway.PricingTable{Models: map[string]modelgateway.ModelPrice{"fixture-model": {}}}, 403},
		{"priority", `{"model":"fixture-model","service_tier":"priority"}`, priced, 403},
		{"project default", `{"model":"fixture-model","service_tier":"auto"}`, priced, 403},
		{"standard", `{"model":"fixture-model","input":[],"service_tier":"default"}`, priced, 200},
		{"native defaults", `{"model":"fixture-model","store":false,"input":[],"tools":[{"type":"tool_search","execution":"client"}]}`, priced, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity := testExecutionGrant()
			limits := identity.Policy.Spec.Budgets[api.CapabilityModelInference]
			limits.MaxCostMicros = 1000
			identity.Policy.Spec.Budgets[api.CapabilityModelInference] = limits
			h := newHandler(t, identity, testModelUpstream)
			h.Config.Pricing = tc.pricing
			calls := 0
			h.Config.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				var request map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || string(request["service_tier"]) != `"default"` {
					t.Fatalf("standard admission missing: %v %v", request, err)
				}
				if strings.Contains(tc.body, "tool_search") && !strings.Contains(string(request["tools"]), "tool_search") {
					t.Fatal("native client tool semantics were removed")
				}
				return modelResponse(r, `{"usage":{"total_tokens":3,"input_tokens":2,"output_tokens":1,"prompt_tokens":2,"completion_tokens":1}}`), nil
			})}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, testResponsesPath, strings.NewReader(tc.body)))
			wantCalls := 0
			if tc.want == 200 {
				wantCalls = 1
			}
			if w.Code != tc.want || calls != wantCalls {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
			}
		})
	}
}

func TestOpenAIModelDiscoveryNeedsNoPriceOrBody(t *testing.T) {
	identity := testExecutionGrant()
	limits := identity.Policy.Spec.Budgets[api.CapabilityModelInference]
	limits.MaxCostMicros = 1000
	identity.Policy.Spec.Budgets[api.CapabilityModelInference] = limits
	h := newHandler(t, identity, testModelUpstream)
	h.Config.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) != 0 {
			t.Fatalf("discovery body changed: %s %v", body, err)
		}
		return modelResponse(r, `{"data":[]}`), nil
	})}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if w.Code != 200 {
		t.Fatalf("discovery denied without pricing: %d %s", w.Code, w.Body.String())
	}
}
