package model_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	modelgateway "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

const (
	testResponseFailedEvent = "response.failed"
)

func TestFailedStreamDiagnosticsPreserveErrorWithoutBypassingUsage(t *testing.T) {
	stream := `data: {"type":"response.output_text.delta","delta":"private-model-output"}` + "\n\n" +
		`data: {"type":"response.failed","response":{"status":"failed","error":{"code":"invalid_value","param":"tools","message":"Tools invalid; bearer sk-test; access_token=secret-record; eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhIn0.signature"}}}` + "\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-ID", "req_fixture")
		_, _ = fmt.Fprint(w, stream)
	}))
	defer server.Close()
	h := newHandler(t, nil, &gateway.FakeRunFinder{Identity: testRunIdentity()}, server.URL)
	var diagnostics []modelgateway.ResponseDiagnostic
	h.Config.Diagnostics = func(d modelgateway.ResponseDiagnostic) { diagnostics = append(diagnostics, d) }
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, testResponsesPath, strings.NewReader(`{"model":"test","stream":true}`)))
	if w.Code != 502 || strings.Contains(w.Body.String(), "private-model-output") {
		t.Fatal("diagnostics bypassed completed-usage verification")
	}
	if len(diagnostics) != 1 {
		t.Fatalf("got %d diagnostics, want one terminal summary", len(diagnostics))
	}
	d := diagnostics[0]
	if d.RequestID != "req_fixture" || d.TerminalEvent != testResponseFailedEvent || d.ErrorCode != "invalid_value" ||
		d.ErrorParam != "tools" || d.ValidationError == "" || !strings.Contains(d.ErrorMessage, "Tools invalid") {
		t.Fatalf("provider failure evidence lost: %+v", d)
	}
	if strings.Contains(fmt.Sprintf("%+v", d), "private-model-output") || strings.Contains(d.ErrorMessage, testOpenAIKey) ||
		strings.Contains(d.ErrorMessage, "secret-record") || strings.Contains(d.ErrorMessage, "eyJhbGci") {
		t.Fatal("diagnostics leaked credentials or model output")
	}
	state := h.Config.BudgetTracker.State(testRunUID)
	if state.UnitsUsed != 10_000 || state.ReservedUnits != 0 {
		t.Fatal("diagnostics weakened conservative settlement")
	}
}

func TestGatewayNormalizesVerifiedHeaderlessSSE(t *testing.T) {
	stream := "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"total_tokens\":12}}}\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Prevent net/http's automatic text/plain content sniffing.
		w.Header()["Content-Type"] = nil
		_, _ = fmt.Fprint(w, stream)
	}))
	defer server.Close()
	h := newHandler(t, nil, &gateway.FakeRunFinder{Identity: testRunIdentity()}, server.URL)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, testResponsesPath, strings.NewReader(`{"model":"test","stream":true}`)))
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/event-stream" || w.Body.String() != stream ||
		h.Config.BudgetTracker.State(testRunUID).UnitsUsed != 12 {
		t.Fatalf("verified headerless SSE not normalized or settled: status=%d", w.Code)
	}
}
