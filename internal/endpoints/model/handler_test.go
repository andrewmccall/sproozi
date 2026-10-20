/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package model_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/andrewmccall/sproozi/internal/budget"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	modelgateway "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

const (
	testModelUpstream = "http://model.invalid"
)

func TestHandlerVerifiesUsageWithClientCompressionPreference(t *testing.T) {
	stream := "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"total_tokens\":12,\"input_tokens\":9,\"output_tokens\":3}}}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		compressed := gzip.NewWriter(w)
		_, _ = compressed.Write([]byte(stream))
		_ = compressed.Close()
	}))
	defer upstream.Close()
	h := newHandler(t, testExecutionGrant(), upstream.URL)
	request := httptest.NewRequest(http.MethodPost, testResponsesPath, strings.NewReader(`{"model":"test","stream":true}`))
	request.Header.Set("Accept-Encoding", "gzip, br, zstd")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	state := observedState(t, h.Config.BudgetTracker, testRunUID)
	if response.Code != 200 || response.Body.String() != stream || state.UnitsUsed != 12 ||
		response.Header().Get("Content-Encoding") != "" {
		t.Fatalf("compressed provider stream was not inspected and settled: status=%d state=%+v", response.Code, state)
	}
}

// newHandler supplies an authenticated grant and isolated in-memory accounting.

const (
	testSAName    = "sproozi-uid-test"
	testNamespace = "default"
	testRunName   = "test-run"
	testRunUID    = "test-run-uid"
	testOpenAIKey = "sk-test"
)

func newHandler(t *testing.T, identity *gateway.ExecutionGrant, upstream string) *identityHandler {
	t.Helper()
	return &identityHandler{
		Config: identityConfig{
			Identity:      identity,
			BudgetTracker: budget.NewTracker(),
			AuditLogger:   audit.NewLogger(io.Discard),
			UpstreamURL:   upstream,
			OpenAIKey:     testOpenAIKey,
			HTTPClient:    http.DefaultClient,
		},
	}
}

// testExecutionGrant returns a fully-populated ExecutionGrant suitable for happy-path tests.
func testExecutionGrant() *gateway.ExecutionGrant {
	return &gateway.ExecutionGrant{Template: gateway.TemplateSnapshot{Name: "tmpl"},
		Policy: gateway.PolicySnapshot{Name: "pol", Generation: 1, Spec: sprooziv1alpha1.AgentPolicySpec{
			AllowedCapabilities: []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityModelInference},
			Budgets:             map[sprooziv1alpha1.CapabilityKind]sprooziv1alpha1.EndpointBudget{sprooziv1alpha1.CapabilityModelInference: {MaxUnits: 10000}},
		}},
		Execution:    gateway.Reference{Backend: gateway.BackendKubernetes, Name: "test-run", Namespace: testNamespace, UID: testRunUID},
		Capabilities: []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityModelInference},
		Authority:    gateway.Reference{Backend: gateway.BackendKubernetes, Name: testSAName}}
}

// assertOpenAIErrorBody verifies that body is a valid OpenAI error JSON envelope
// with a non-empty message field.
func assertOpenAIErrorBody(t *testing.T, body []byte) {
	t.Helper()
	var resp struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Errorf("response body is not OpenAI error JSON: %v (body: %s)", err, body)
		return
	}
	if resp.Error.Message == "" {
		t.Error("OpenAI error body missing message")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func modelResponse(req *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     http.StatusText(http.StatusOK),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func TestHandlerMissingGatewayIdentityReturns401(t *testing.T) {
	h := &identityHandler{Config: identityConfig{BudgetTracker: budget.NewTracker(), AuditLogger: audit.NewLogger(io.Discard)}}

	// A workload-supplied bearer token is not a gateway identity.
	for _, authorization := range []string{"", "Bearer bad-token"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("Authorization %q: status = %d, want 401", authorization, rr.Code)
		}
		assertOpenAIErrorBody(t, rr.Body.Bytes())
	}
}

// A stream is relayed as it arrives. One that ends without its terminal event
// has already reached the workload, so it is charged the estimate E: request
// bytes ÷ 4 for input and the visible output bytes, as no time elapsed.
func TestHandlerRelaysResponsesStreamAndSettlesUsage(t *testing.T) {
	stream := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"total_tokens\":12,\"input_tokens\":9,\"output_tokens\":3}}}\n\n"
	request := `{"model":"test","stream":true}`
	for _, valid := range []bool{true, false} {
		body := stream
		if !valid {
			body = strings.Split(stream, "event: response.completed")[0]
		}
		h := newHandler(t, testExecutionGrant(), "https://model.test")
		h.Config.Now = fixedClock()
		h.Config.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			response := modelResponse(r, body)
			response.Header.Set("Content-Type", "text/event-stream")
			return response, nil
		})}
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, testResponsesPath, strings.NewReader(request)))
		state := observedState(t, h.Config.BudgetTracker, testRunUID)
		want := int64(12)
		if !valid {
			want = int64((len(request)+3)/4 + len("hello"))
		}
		if recorder.Code != 200 || recorder.Body.String() != body || state.UnitsUsed != want || state.ReservedUnits != 0 {
			t.Fatalf("valid=%v: status=%d body=%q state=%+v, want %d units", valid, recorder.Code, recorder.Body, state, want)
		}
	}
}

func TestHandlerDisallowedPathReturns403(t *testing.T) {

	rf := testExecutionGrant()
	h := newHandler(t, rf, "")

	req := httptest.NewRequest(http.MethodPost, "/v1/unknown", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	assertOpenAIErrorBody(t, rr.Body.Bytes())
}

func TestHandlerBudgetExhaustedReturns429(t *testing.T) {
	identity := testExecutionGrant()
	bt := budget.NewTracker()
	if err := bt.Meter(identity.Execution.UID, identity.Policy.Spec.Budgets[sprooziv1alpha1.CapabilityModelInference]).Spend(t.Context(), 10000, 0); err != nil {
		t.Fatal(err)
	}

	h := &identityHandler{
		Config: identityConfig{
			Identity:      testExecutionGrant(),
			BudgetTracker: bt,
			AuditLogger:   audit.NewLogger(io.Discard),
			UpstreamURL:   "",
			OpenAIKey:     testOpenAIKey,
			HTTPClient:    http.DefaultClient,
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rr.Code)
	}
	assertOpenAIErrorBody(t, rr.Body.Bytes())
}

func TestHandlerForwardsToUpstreamAndStripsAuth(t *testing.T) {
	var capturedAuthHeader string

	rf := testExecutionGrant()
	h := newHandler(t, rf, testModelUpstream)
	h.Config.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		capturedAuthHeader = req.Header.Get("Authorization")
		return modelResponse(req,
			`{"id":"chatcmpl-1","usage":{"total_tokens":42,"prompt_tokens":30,"completion_tokens":12}}`), nil
	})}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer client-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if capturedAuthHeader != "Bearer sk-test" {
		t.Errorf("upstream Authorization = %q, want %q", capturedAuthHeader, "Bearer sk-test")
	}
	respBody := rr.Body.String()
	if !strings.Contains(respBody, "chatcmpl-1") {
		t.Errorf("response body missing expected content, got: %s", respBody)
	}
}

// captureWriter is a test double for audit.Writer that records all events.
// Like audit.Logger, it accepts concurrent calls.
type captureWriter struct {
	mu     sync.Mutex
	events []audit.Event
}

func (c *captureWriter) Log(e audit.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func TestHandlerSuccessRecordsBudgetUsage(t *testing.T) {
	bt := budget.NewTracker()
	h := &identityHandler{
		Config: identityConfig{
			Identity:      testExecutionGrant(),
			BudgetTracker: bt,
			AuditLogger:   audit.NewLogger(io.Discard),
			UpstreamURL:   testModelUpstream,
			OpenAIKey:     testOpenAIKey,
			HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return modelResponse(req,
					`{"id":"x","usage":{"total_tokens":42,"prompt_tokens":10,"completion_tokens":32}}`), nil
			})},
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer some-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	s := observedState(t, bt, testRunUID) // policyGen=1 matches testExecutionGrant
	if s.UnitsUsed != 42 {
		t.Errorf("TokensUsed = %d, want 42", s.UnitsUsed)
	}
}

func TestHandlerPositiveCostBudgetUsesAdministratorPricing(t *testing.T) {
	identity := testExecutionGrant()
	limits := identity.Policy.Spec.Budgets[sprooziv1alpha1.CapabilityModelInference]
	limits.MaxCostMicros = 500_000
	identity.Policy.Spec.Budgets[sprooziv1alpha1.CapabilityModelInference] = limits
	bt := budget.NewTracker()
	h := &identityHandler{
		Config: identityConfig{
			Identity:      identity,
			BudgetTracker: bt,
			Pricing: &modelgateway.PricingTable{Models: map[string]modelgateway.ModelPrice{
				"gpt-5.3-codex": {
					InputMicrosPerMillionTokens:       1_750_000,
					CachedInputMicrosPerMillionTokens: 175_000,
					OutputMicrosPerMillionTokens:      14_000_000,
				},
			}},
			AuditLogger: audit.NewLogger(io.Discard),
			UpstreamURL: testModelUpstream,
			OpenAIKey:   testOpenAIKey,
			HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return modelResponse(req,
					`{"id":"x","usage":{"total_tokens":42,"prompt_tokens":10,"completion_tokens":32}}`), nil
			})},
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-5.3-codex","messages":[]}`))
	req.Header.Set("Authorization", "Bearer some-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	state := observedState(t, bt, testRunUID)
	if state.UnitsUsed != 42 || state.CostMicros != 466 {
		t.Fatalf("budget state = %+v, want 42 tokens and 466 cost micros", state)
	}
}

func TestHandlerUncappedCostBudgetDoesNotRequirePricingForEveryModel(t *testing.T) {
	h := &identityHandler{
		Config: identityConfig{
			Identity:      testExecutionGrant(),
			BudgetTracker: budget.NewTracker(),
			Pricing:       &modelgateway.PricingTable{Models: map[string]modelgateway.ModelPrice{}},
			AuditLogger:   audit.NewLogger(io.Discard),
			UpstreamURL:   testModelUpstream,
			OpenAIKey:     testOpenAIKey,
			HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return modelResponse(req,
					`{"id":"x","usage":{"total_tokens":10}}`), nil
			})},
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"unlisted-model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer some-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
}

func TestHandlerAuditLogsAllowedRequest(t *testing.T) {
	capture := &captureWriter{}
	h := &identityHandler{
		Config: identityConfig{
			Identity:      testExecutionGrant(),
			BudgetTracker: budget.NewTracker(),
			AuditLogger:   capture,
			UpstreamURL:   testModelUpstream,
			OpenAIKey:     testOpenAIKey,
			HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return modelResponse(req,
					`{"id":"x","usage":{"total_tokens":10,"prompt_tokens":5,"completion_tokens":5}}`), nil
			})},
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer some-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(capture.events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(capture.events))
	}
	e := capture.events[0]
	if !e.Allowed {
		t.Errorf("Allowed = false, want true")
	}
	if e.TokensUsed != 10 {
		t.Errorf("TokensUsed = %d, want 10", e.TokensUsed)
	}
	if e.RunID != testNamespace+"/"+testRunName {
		t.Errorf("RunID = %q, want %q", e.RunID, testNamespace+"/"+testRunName)
	}
}

func TestHandlerRejectsStreamingRequest(t *testing.T) {

	rf := testExecutionGrant()
	h := newHandler(t, rf, "http://unused")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[],"stream":true}`))
	req.Header.Set("Authorization", "Bearer some-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for streaming", w.Code)
	}
}

// TestHandlerUsageAboveReservationExhaustsAllowance verifies that trusted usage
// above a small output reservation is recorded up to the allowance. Keeping
// only the reservation would let a large input repeat without limit. The
// output has already reached the workload, so the call is audited as exceeded.
func TestHandlerUsageAboveReservationExhaustsAllowance(t *testing.T) {
	identity := testExecutionGrant()
	limits := identity.Policy.Spec.Budgets[sprooziv1alpha1.CapabilityModelInference]
	limits.MaxUnits = 100
	identity.Policy.Spec.Budgets[sprooziv1alpha1.CapabilityModelInference] = limits
	var calls atomic.Int32
	h := newHandler(t, identity, testModelUpstream)
	capture := &captureWriter{}
	h.Config.AuditLogger = capture
	h.Config.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return modelResponse(req,
			`{"id":"x","usage":{"total_tokens":5016,"prompt_tokens":5000,"completion_tokens":16}}`), nil
	})}

	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4o","messages":[],"max_tokens":16}`))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if w := send(); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"x"`) {
		t.Fatalf("status = %d, body = %s; want the relayed response", w.Code, w.Body.String())
	}
	state := observedState(t, h.Config.BudgetTracker, testRunUID)
	if state.UnitsUsed != 100 || state.ReservedUnits != 0 {
		t.Fatalf("budget state = %+v, want the allowance of 100 used and no reservation", state)
	}
	if e := capture.events[0]; !e.Allowed || e.EndReason != "exceeded" || e.UsageSource != "provider" || e.TokensUsed != 5016 {
		t.Fatalf("audit = %+v, want an exceeded call settled from provider usage", e)
	}
	if w := send(); w.Code != http.StatusTooManyRequests || calls.Load() != 1 {
		t.Fatalf("second request status = %d after %d provider calls, want 429 after 1", w.Code, calls.Load())
	}
}

// A JSON response without usage has reached the workload; it is charged the
// estimate, here request bytes ÷ 4 plus the visible body bytes.
func TestHandlerMissingUsageChargesTheEstimate(t *testing.T) {
	identity := testExecutionGrant()
	limits := identity.Policy.Spec.Budgets[sprooziv1alpha1.CapabilityModelInference]
	limits.MaxUnits = 100
	identity.Policy.Spec.Budgets[sprooziv1alpha1.CapabilityModelInference] = limits
	var calls atomic.Int32
	h := newHandler(t, identity, testModelUpstream)
	h.Config.Now = fixedClock()
	h.Config.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return modelResponse(req, `{"id":"x"}`), nil
	})}
	body := `{"model":"gpt-4o","messages":[],"max_tokens":25}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer some-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK || w.Body.String() != `{"id":"x"}` || calls.Load() != 1 {
		t.Fatalf("status = %d, body = %s, calls = %d", w.Code, w.Body.String(), calls.Load())
	}
	state := observedState(t, h.Config.BudgetTracker, testRunUID)
	if want := int64((len(body)+3)/4 + len(`{"id":"x"}`)); state.UnitsUsed != want || state.ReservedUnits != 0 {
		t.Fatalf("budget state = %+v, want %d estimated and no reservation", state, want)
	}
}

// A response event larger than the parser's bound is still relayed, but no
// usage can be read from it, so the reservation is charged in full: with no
// output bound that is all remaining capacity. The operator log names the
// run, the reason and the charge.
func TestOversizedResponseChargesTheReservationAndLogsIt(t *testing.T) {
	identity := testExecutionGrant()
	limits := identity.Policy.Spec.Budgets[sprooziv1alpha1.CapabilityModelInference]
	limits.MaxUnits = 100
	identity.Policy.Spec.Budgets[sprooziv1alpha1.CapabilityModelInference] = limits
	var logs bytes.Buffer
	h := newHandler(t, identity, testModelUpstream)
	h.Config.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	large := `{"id":"` + strings.Repeat("x", 8<<20) + `"}`
	h.Config.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return modelResponse(req, large), nil
	})}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[]}`)))
	if w.Code != http.StatusOK || w.Body.Len() != len(large) {
		t.Fatalf("status = %d, relayed %d of %d bytes", w.Code, w.Body.Len(), len(large))
	}
	if state := observedState(t, h.Config.BudgetTracker, testRunUID); state.UnitsUsed != 100 || state.ReservedUnits != 0 {
		t.Fatalf("budget state = %+v, want all 100 units charged", state)
	}
	for _, want := range []string{`msg="Charged model reservation in full"`, "runUID=" + testRunUID,
		`detail="provider event exceeded the parse limit"`, "endReason=parse_limit", "units=100", "costMicros=0"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log %q lacks %q", logs.String(), want)
		}
	}
}
