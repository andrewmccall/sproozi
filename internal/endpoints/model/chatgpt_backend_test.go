package model_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	model "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

const chatGPTSignIn = "eyJ-codex-own-chatgpt-access-token"

// A ChatGPT-signed-in Codex reaches only its model calls, which are counted,
// and the account and model metadata it needs; ChatGPT's own connectors and
// plugins are refused (ADR 0047).
func TestChatGPTBackendCountsModelCallsAndRefusesConnectors(t *testing.T) {
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer "+chatGPTSignIn || r.Header.Get("ChatGPT-Account-Id") != "acct" {
			t.Errorf("%s lost the harness's sign-in or account", r.URL.Path)
		}
		if r.URL.Path == "/backend-api/codex/responses" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, completedStream)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"accounts":[]}`)
	}))
	defer upstream.Close()
	tracker := budget.NewTracker()
	h := model.NewChatGPTBackendHandler(model.HandlerConfig{ProviderAuth: modelauth.Workload{}, UpstreamURL: upstream.URL,
		HTTPClient: upstream.Client(), AuditLogger: audit.NewLogger(io.Discard)})
	routes := workloadRoutes(model.ChatGPTBackendAuthority, h, tracker)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://chatgpt.com"+path, strings.NewReader(body))
		r = r.WithContext(gateway.WithIdentity(r.Context(), testExecutionGrant()))
		r.Header.Set("Authorization", "Bearer "+chatGPTSignIn)
		r.Header.Set("ChatGPT-Account-Id", "acct")
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, r)
		return response
	}
	if response := call(http.MethodPost, "/backend-api/codex/responses", `{"model":"gpt-fixture","stream":true,"store":false}`); response.Code != http.StatusOK {
		t.Fatalf("model call: %d %s", response.Code, response.Body.String())
	}
	if state := observedState(t, tracker, testRunUID+"/model.inference"); state.UnitsUsed != 12 {
		t.Fatalf("the model call wasn't counted: %+v", state)
	}
	for _, path := range []string{"/backend-api/codex/models?client_version=1", "/backend-api/wham/accounts/check", "/backend-api/wham/settings/user"} {
		if response := call(http.MethodGet, path, ""); response.Code != http.StatusOK {
			t.Fatalf("metadata %s: %d", path, response.Code)
		}
	}
	before := len(paths)
	for _, refused := range []struct{ method, path string }{{http.MethodPost, "/backend-api/ps/mcp"}, {http.MethodGet, "/backend-api/ps/plugins/list"},
		{http.MethodPost, "/backend-api/codex/analytics-events/events"}, {http.MethodGet, "/backend-api/wham/usage"}} {
		if response := call(refused.method, refused.path, "{}"); response.Code != http.StatusForbidden {
			t.Fatalf("%s %s was relayed: %d", refused.method, refused.path, response.Code)
		}
	}
	if len(paths) != before || paths[0] != "POST /backend-api/codex/responses" {
		t.Fatalf("upstream saw %q", paths)
	}
}

func TestProviderHostsCoverSignedInRoutesOnlyWhenTheHarnessSignsIn(t *testing.T) {
	openai, _ := model.ProviderFor("openai")
	if got := strings.Join(openai.Hosts(false), ","); got != "api.openai.com" {
		t.Fatalf("gateway-held key hosts: %s", got)
	}
	if got := strings.Join(openai.Hosts(true), ","); got != "api.openai.com,chatgpt.com,auth.openai.com" {
		t.Fatalf("signed-in hosts: %s", got)
	}
	anthropic, _ := model.ProviderFor("anthropic")
	if got := strings.Join(anthropic.Hosts(true), ","); got != "api.anthropic.com,platform.claude.com" {
		t.Fatalf("anthropic signed-in hosts: %s", got)
	}
}
