package model_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"

	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

type chatGPTStore struct{ unavailable bool }

func (s chatGPTStore) Load(context.Context) (modelauth.Session, error) {
	scopes := []string{modelauth.PlanScope}
	if s.unavailable {
		scopes = nil
	}
	return modelauth.Session{Issuer: modelauth.Issuer, Subject: "account", ClientID: "oaiapp_fixture", HostID: "host",
		AccessToken: "fixture-oauth-access", RefreshToken: "fixture-refresh", IDToken: "fixture-id", TokenType: "Bearer",
		ExpiresIn: 3600, SavedAt: time.Now().UTC(), Scopes: scopes}, nil
}
func (chatGPTStore) Replace(context.Context, modelauth.Session, modelauth.Session) error { return nil }

func TestChatGPTGatewayAuthenticatesSettlesAndAuditsResponses(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Header.Get("Authorization") != "Bearer fixture-oauth-access" || r.Header.Get("ChatGPT-Account-ID") != "" {
			t.Error("gateway did not isolate provider authentication from workload credentials/account hints")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"total_tokens\":12,\"input_tokens\":9,\"output_tokens\":3}}}\n\n"))
	}))
	defer server.Close()
	h := newHandler(t, nil, &gateway.FakeRunFinder{Identity: testRunIdentity()}, server.URL)
	h.Config.ProviderAuth = modelauth.NewChatGPT(chatGPTStore{}, http.DefaultClient)
	var logs bytes.Buffer
	h.Config.AuditLogger = audit.NewLogger(&logs)
	r := httptest.NewRequest(http.MethodPost, testResponsesPath, strings.NewReader(`{"model":"gpt-5.3-codex","store":false,"stream":true,"input":[]}`))
	r.Header.Set("Authorization", "Bearer workload-placeholder")
	r.Header.Set("ChatGPT-Account-ID", "workload-selected-account")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !called || h.Config.BudgetTracker.State(testRunUID).UnitsUsed != 12 {
		t.Fatalf("ChatGPT inference did not settle: %d", w.Code)
	}
	if !strings.Contains(logs.String(), `"providerAuthMode":"chatgpt"`) || strings.Contains(logs.String(), "fixture-oauth") {
		t.Fatal("auth-mode evidence is missing or audit leaked credentials")
	}
}

func TestChatGPTGatewayRejectsIncompatibleRequestsBeforeUpstream(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		cost             int64
		unavailable      bool
		status           int
	}{
		{"chat completions", "/v1/chat/completions", `{"model":"test"}`, 0, false, 403},
		{"stored responses", testResponsesPath, `{"store":true,"stream":true,"input":[]}`, 0, false, 403},
		{"non-streaming", testResponsesPath, `{"store":false,"stream":false,"input":[]}`, 0, false, 403},
		{"output bound unsupported", testResponsesPath, `{"store":false,"stream":true,"input":[],"max_output_tokens":100}`, 0, false, 403},
		{"previous response id", testResponsesPath, `{"store":false,"stream":true,"input":[],"previous_response_id":"resp_1"}`, 0, false, 403},
		{"string input", testResponsesPath, `{"store":false,"stream":true,"input":"hello"}`, 0, false, 403},
		{"API dollar cap", testResponsesPath, `{"store":false,"stream":true,"input":[]}`, 100, false, 403},
		{"identity without plan scope", testResponsesPath, `{"store":false,"stream":true,"input":[]}`, 0, true, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("denied request reached upstream") }))
			defer server.Close()
			h := newHandler(t, nil, &gateway.FakeRunFinder{Identity: testRunIdentity()}, server.URL)
			limits := h.Config.Identity.Policy.Spec.Budgets[sprooziv1alpha1.CapabilityModelInference]
			limits.MaxCostMicros = tc.cost
			h.Config.Identity.Policy.Spec.Budgets[sprooziv1alpha1.CapabilityModelInference] = limits
			h.Config.ProviderAuth = modelauth.NewChatGPT(chatGPTStore{unavailable: tc.unavailable}, http.DefaultClient)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
			state := h.Config.BudgetTracker.State(testRunUID)
			if w.Code != tc.status || state.UnitsUsed != 0 || state.ReservedUnits != 0 {
				t.Fatalf("invalid request reached provider or consumed tokens: %d", w.Code)
			}
		})
	}
}
