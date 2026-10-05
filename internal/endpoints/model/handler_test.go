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
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/andrewmccall/sproozi/internal/budget"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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
	h := newHandler(t, nil, &gateway.FakeRunFinder{Identity: testRunIdentity()}, upstream.URL)
	request := httptest.NewRequest(http.MethodPost, testResponsesPath, strings.NewReader(`{"model":"test","stream":true}`))
	request.Header.Set("Accept-Encoding", "gzip, br, zstd")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	state := h.Config.BudgetTracker.State(testRunUID)
	if response.Code != 200 || response.Body.String() != stream || state.UnitsUsed != 12 ||
		response.Header().Get("Content-Encoding") != "" {
		t.Fatalf("compressed provider stream was not inspected and settled: status=%d state=%+v", response.Code, state)
	}
}

// newHandler creates a Handler wired with the given token validator, run finder,
// and upstream URL. BudgetTracker and AuditLogger use safe no-op defaults.

const (
	testSAName    = "sproozi-uid-test"
	testNamespace = "default"
	testRunName   = "test-run"
	testRunUID    = "test-run-uid"
	testOpenAIKey = "sk-test"
)

func newHandler(t *testing.T, _ gateway.TokenValidator, rf gateway.RunFinder, upstream string) *identityHandler {
	t.Helper()
	return &identityHandler{
		Config: identityConfig{
			Identity:      rf.(*gateway.FakeRunFinder).Identity,
			BudgetTracker: budget.NewTracker(),
			AuditLogger:   audit.NewLogger(io.Discard),
			UpstreamURL:   upstream,
			OpenAIKey:     testOpenAIKey,
			HTTPClient:    http.DefaultClient,
		},
	}
}

// testRunIdentity returns a fully-populated RunIdentity suitable for happy-path tests.
func testRunIdentity() *gateway.RunIdentity {
	return &gateway.RunIdentity{
		SAName: testSAName,
		Run: &sprooziv1alpha1.AgentRun{
			ObjectMeta: metav1.ObjectMeta{Name: "test-run", Namespace: testNamespace, UID: types.UID(testRunUID)},
			Spec: sprooziv1alpha1.AgentRunSpec{
				Capabilities: []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityModelInference},
			},
		},
		Template: &sprooziv1alpha1.AgentTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: testNamespace},
		},
		Policy: &sprooziv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "pol", Namespace: testNamespace, Generation: 1},
			Spec: sprooziv1alpha1.AgentPolicySpec{
				AllowedCapabilities: []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityModelInference},
				Budgets:             map[sprooziv1alpha1.CapabilityKind]sprooziv1alpha1.EndpointBudget{sprooziv1alpha1.CapabilityModelInference: {MaxUnits: 10000}},
			},
		},
	}
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

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	assertOpenAIErrorBody(t, rr.Body.Bytes())
}

func TestHandlerBuffersResponsesStreamAndSettlesUsageBeforeReturning(t *testing.T) {
	stream := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"total_tokens\":12,\"input_tokens\":9,\"output_tokens\":3}}}\n\n"
	for _, valid := range []bool{true, false} {
		body := stream
		if !valid {
			body = strings.Split(stream, "event: response.completed")[0]
		}
		h := newHandler(t, nil, &gateway.FakeRunFinder{Identity: testRunIdentity()}, "https://model.test")
		h.Config.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			response := modelResponse(r, body)
			response.Header.Set("Content-Type", "text/event-stream")
			return response, nil
		})}
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, testResponsesPath, strings.NewReader(`{"model":"test","stream":true}`)))
		state := h.Config.BudgetTracker.State(testRunUID)
		if valid {
			if recorder.Code != 200 || recorder.Body.String() != stream || state.UnitsUsed != 12 || state.ReservedUnits != 0 {
				t.Fatalf("valid stream: status=%d state=%+v", recorder.Code, state)
			}
		} else if recorder.Code != 502 || strings.Contains(recorder.Body.String(), "delta") || state.UnitsUsed != 10000 {
			t.Fatalf("incomplete stream escaped or failed to charge reservation: status=%d state=%+v", recorder.Code, state)
		}
	}
}

func TestHandlerInvalidTokenReturns401(t *testing.T) {
	h := &identityHandler{Config: identityConfig{BudgetTracker: budget.NewTracker(), AuditLogger: audit.NewLogger(io.Discard)}}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer bad-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	assertOpenAIErrorBody(t, rr.Body.Bytes())
}

func TestHandlerRunNotFoundReturns403(t *testing.T) {
	h := &identityHandler{Config: identityConfig{BudgetTracker: budget.NewTracker(), AuditLogger: audit.NewLogger(io.Discard)}}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	assertOpenAIErrorBody(t, rr.Body.Bytes())
}

func TestHandlerDisallowedPathReturns403(t *testing.T) {
	tv := &gateway.FakeTokenValidator{SAName: testSAName}
	rf := &gateway.FakeRunFinder{Identity: testRunIdentity()}
	h := newHandler(t, tv, rf, "")

	req := httptest.NewRequest(http.MethodPost, "/v1/unknown", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	assertOpenAIErrorBody(t, rr.Body.Bytes())
}

func TestHandlerCapabilityNotGrantedReturns403(t *testing.T) {
	identity := testRunIdentity()
	identity.Run.Spec.Capabilities = []sprooziv1alpha1.CapabilityKind{}

	tv := &gateway.FakeTokenValidator{SAName: testSAName}
	rf := &gateway.FakeRunFinder{Identity: identity}
	h := newHandler(t, tv, rf, "")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	assertOpenAIErrorBody(t, rr.Body.Bytes())
}

func TestHandlerBudgetExhaustedReturns429(t *testing.T) {
	identity := testRunIdentity()
	bt := budget.NewTracker()
	if err := bt.Meter(string(identity.Run.UID), identity.Policy.Spec.Budgets[sprooziv1alpha1.CapabilityModelInference]).Spend(10000, 0); err != nil {
		t.Fatal(err)
	}

	h := &identityHandler{
		Config: identityConfig{
			Identity:      testRunIdentity(),
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

	tv := &gateway.FakeTokenValidator{SAName: testSAName}
	rf := &gateway.FakeRunFinder{Identity: testRunIdentity()}
	h := newHandler(t, tv, rf, testModelUpstream)
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
type captureWriter struct {
	events []audit.Event
}

func (c *captureWriter) Log(e audit.Event) {
	c.events = append(c.events, e)
}

func TestHandlerSuccessRecordsBudgetUsage(t *testing.T) {
	bt := budget.NewTracker()
	h := &identityHandler{
		Config: identityConfig{
			Identity:      testRunIdentity(),
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
	s := bt.State(testRunUID) // policyGen=1 matches testRunIdentity
	if s.UnitsUsed != 42 {
		t.Errorf("TokensUsed = %d, want 42", s.UnitsUsed)
	}
}

func TestHandlerPositiveCostBudgetUsesAdministratorPricing(t *testing.T) {
	identity := testRunIdentity()
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
	state := bt.State(testRunUID)
	if state.UnitsUsed != 42 || state.CostMicros != 466 {
		t.Fatalf("budget state = %+v, want 42 tokens and 466 cost micros", state)
	}
}

func TestHandlerUncappedCostBudgetDoesNotRequirePricingForEveryModel(t *testing.T) {
	h := &identityHandler{
		Config: identityConfig{
			Identity:      testRunIdentity(),
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
			Identity:      testRunIdentity(),
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
	tv := &gateway.FakeTokenValidator{SAName: testSAName}
	rf := &gateway.FakeRunFinder{Identity: testRunIdentity()}
	h := newHandler(t, tv, rf, "http://unused")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[],"stream":true}`))
	req.Header.Set("Authorization", "Bearer some-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for streaming", w.Code)
	}
}

func TestHandlerMissingUsageConsumesReservationAndDenies(t *testing.T) {
	identity := testRunIdentity()
	limits := identity.Policy.Spec.Budgets[sprooziv1alpha1.CapabilityModelInference]
	limits.MaxUnits = 100
	identity.Policy.Spec.Budgets[sprooziv1alpha1.CapabilityModelInference] = limits
	var calls atomic.Int32
	h := newHandler(t, &gateway.FakeTokenValidator{SAName: testSAName},
		&gateway.FakeRunFinder{Identity: identity}, testModelUpstream)
	h.Config.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return modelResponse(req, `{"id":"x"}`), nil
	})}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[],"max_tokens":25}`))
	req.Header.Set("Authorization", "Bearer some-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "usage_unavailable") {
		t.Errorf("body = %s, want usage_unavailable", w.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
	state := h.Config.BudgetTracker.State(testRunUID)
	if state.UnitsUsed != 25 || state.ReservedUnits != 0 {
		t.Fatalf("budget state = %+v, want 25 consumed and no reservation", state)
	}
}

func TestHandlerConcurrentRequestsReserveBeforeForwarding(t *testing.T) {
	identity := testRunIdentity()
	limits := identity.Policy.Spec.Budgets[sprooziv1alpha1.CapabilityModelInference]
	limits.MaxUnits = 100
	identity.Policy.Spec.Budgets[sprooziv1alpha1.CapabilityModelInference] = limits
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	h := newHandler(t, &gateway.FakeTokenValidator{SAName: testSAName},
		&gateway.FakeRunFinder{Identity: identity}, testModelUpstream)
	h.Config.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return modelResponse(req,
			`{"id":"x","usage":{"total_tokens":20}}`), nil
	})}

	firstResponse := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4o","messages":[],"max_tokens":60}`))
		req.Header.Set("Authorization", "Bearer first")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		firstResponse <- w
	}()
	<-started

	secondReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[],"max_tokens":60}`))
	secondReq.Header.Set("Authorization", "Bearer second")
	secondResponse := httptest.NewRecorder()
	h.ServeHTTP(secondResponse, secondReq)
	if secondResponse.Code != http.StatusTooManyRequests {
		t.Fatalf("concurrent second status = %d, want 429; body = %s",
			secondResponse.Code, secondResponse.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls before first settles = %d, want 1", calls.Load())
	}

	close(release)
	first := <-firstResponse
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200; body = %s", first.Code, first.Body.String())
	}
	state := h.Config.BudgetTracker.State(testRunUID)
	if state.UnitsUsed != 20 || state.ReservedUnits != 0 {
		t.Fatalf("budget state = %+v, want 20 consumed and no reservation", state)
	}
}
