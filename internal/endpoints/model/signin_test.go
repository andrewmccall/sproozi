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
)

const refreshBody = `{"grant_type":"refresh_token","refresh_token":"sk-ant-ort01-harness-refresh","client_id":"claude-code"}`

func signInRequest(method, target, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Cookie", "browser-session")
	return r.WithContext(gateway.WithIdentity(r.Context(), testExecutionGrant()))
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
