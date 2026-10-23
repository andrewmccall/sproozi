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

const copilotGitHubToken = "gho_copilot-harness-own-sign-in"

// A Copilot signed in to GitHub reaches its model calls in each protocol,
// which are counted, and its model list; GitHub's built-in MCP server and
// agents are refused (ADR 0047).
func TestCopilotCountsEveryModelProtocolAndRefusesItsSideChannels(t *testing.T) {
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer "+copilotGitHubToken || r.Header.Get("Copilot-Integration-Id") != "copilot-developer-cli" {
			t.Errorf("%s lost the sign-in or Copilot's headers", r.URL.Path)
		}
		switch r.URL.Path {
		case "/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, completedStream)
		case "/v1/messages":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, anthropicSuccess)
		case "/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[]}`)
		}
	}))
	defer upstream.Close()
	tracker := budget.NewTracker()
	h := model.NewCopilotHandler(model.HandlerConfig{ProviderAuth: modelauth.Workload{}, UpstreamURL: upstream.URL,
		HTTPClient: upstream.Client(), AuditLogger: audit.NewLogger(io.Discard)})
	routes := workloadRoutes(model.CopilotIndividualAuthority, h, tracker)
	call := func(method, path, body string) int {
		r := httptest.NewRequest(method, "https://api.individual.githubcopilot.com"+path, strings.NewReader(body))
		r = r.WithContext(gateway.WithIdentity(r.Context(), testExecutionGrant()))
		r.Header.Set("Authorization", "Bearer "+copilotGitHubToken)
		r.Header.Set("Copilot-Integration-Id", "copilot-developer-cli")
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, r)
		return response.Code
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/responses", `{"model":"gpt-fixture","stream":true}`},
		{http.MethodPost, "/v1/messages", `{"model":"claude-fixture","max_tokens":2,"messages":[{"role":"user","content":"hi"}]}`},
		{http.MethodPost, "/chat/completions", `{"model":"gemini-fixture","messages":[{"role":"user","content":"hi"}]}`},
		{http.MethodGet, "/models", ""},
		{http.MethodPost, "/auto", "{}"},
	} {
		if code := call(c.method, c.path, c.body); code != http.StatusOK {
			t.Fatalf("%s %s: %d", c.method, c.path, code)
		}
	}
	if state := observedState(t, tracker, testRunUID+"/model.inference"); state.UnitsUsed != 12+22+5 {
		t.Fatalf("model calls weren't all counted: %+v", state)
	}
	before := len(seen)
	for _, c := range []struct{ method, path string }{{http.MethodPost, "/mcp/readonly"},
		{http.MethodGet, "/agents/swe/internal/memory/v0/enabled"}, {http.MethodPost, "/telemetry"}} {
		if code := call(c.method, c.path, "{}"); code != http.StatusForbidden {
			t.Fatalf("%s %s was relayed: %d", c.method, c.path, code)
		}
	}
	if len(seen) != before {
		t.Fatalf("refused requests reached Copilot: %q", seen[before:])
	}
}
