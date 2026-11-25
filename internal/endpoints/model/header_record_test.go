package model_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	model "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

// Media type, header, call name and Copilot path the header record repeats.
const (
	jsonMediaType       = "application/json"
	contentTypeHeader   = "Content-Type"
	chatCompletionsCall = "chat completions"
	nonSuccessCall      = "non-2xx"
	copilotChatPath     = "/chat/completions"
)

// clientHeaders are what a harness sends with every recorded call: its own
// credentials, the gateway's proxy credential, a cookie, protocol headers of
// both providers and headers of its own.
var clientHeaders = map[string]string{
	"Accept":                 jsonMediaType,
	"Accept-Encoding":        gzipEncoding,
	"Anthropic-Beta":         "client-beta",
	"Anthropic-Version":      "2099-01-01",
	"Authorization":          "Bearer client-token",
	contentTypeHeader:        jsonMediaType,
	"Cookie":                 "session=client",
	"Copilot-Integration-Id": "client-integration",
	"Openai-Beta":            "client-openai-beta",
	"Proxy-Authorization":    "Bearer gateway-token",
	"User-Agent":             "client-agent/1",
	"X-Api-Key":              "client-key",
	"X-Client-Custom":        "client-value",
}

// providerHeaders are what the provider answers every recorded call with.
var providerHeaders = map[string]string{
	"Anthropic-Ratelimit-Requests-Remaining": "9",
	contentTypeHeader:                        jsonMediaType,
	"Openai-Organization":                    "org-fixture",
	"Openai-Project":                         "proj-fixture",
	"Request-Id":                             "req_provider",
	"Retry-After":                            "1",
	"Set-Cookie":                             "provider=1",
	"Set-Cookie2":                            "provider=2",
	"X-Provider-Custom":                      "provider-value",
	"X-Request-Id":                           "x-req-provider",
}

// headerCall is one recorded call on a model authority.
type headerCall struct {
	name, method, path, body string
	status                   int
	answer                   string
	// apiKey authenticates with a gateway-held key instead of forwarding
	// the harness's own credential.
	apiKey bool
}

// headerCalls are the calls recorded on each model authority of the provider
// table. A new authority fails the record until it has its own.
func headerCalls() map[string][]headerCall {
	openAI := []headerCall{
		{name: chatCompletionsCall, method: http.MethodPost, path: chatPath, body: chatBody, status: http.StatusOK, answer: chatSuccess},
		{name: chatCompletionsCall + ", gateway-held key", method: http.MethodPost, path: chatPath, body: chatBody,
			status: http.StatusOK, answer: chatSuccess, apiKey: true},
		{name: nonSuccessCall, method: http.MethodPost, path: chatPath, body: chatBody,
			status: http.StatusTooManyRequests, answer: `{"error":{"message":"slow down"}}`},
		{name: "model discovery", method: http.MethodGet, path: testModelsPath, status: http.StatusOK, answer: `{"data":[]}`},
	}
	anthropic := []headerCall{
		{name: "messages", method: http.MethodPost, path: testMessagesPath, body: messagesBody, status: http.StatusOK, answer: anthropicSuccess},
		{name: "messages, gateway-held key", method: http.MethodPost, path: testMessagesPath, body: messagesBody,
			status: http.StatusOK, answer: anthropicSuccess, apiKey: true},
		{name: nonSuccessCall, method: http.MethodPost, path: testMessagesPath, body: messagesBody,
			status: http.StatusTooManyRequests, answer: `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`},
	}
	copilot := []headerCall{
		{name: "messages", method: http.MethodPost, path: testMessagesPath, body: messagesBody, status: http.StatusOK, answer: anthropicSuccess},
		{name: chatCompletionsCall, method: http.MethodPost, path: copilotChatPath, body: chatBody, status: http.StatusOK, answer: chatSuccess},
	}
	return map[string][]headerCall{
		model.OpenAIAuthority: openAI,
		model.ChatGPTBackendAuthority: {{name: "responses", method: http.MethodPost, path: "/backend-api/codex/responses",
			body: `{"model":"test","input":[]}`, status: http.StatusOK,
			answer: `{"usage":{"total_tokens":7,"input_tokens":5,"output_tokens":2}}`}},
		model.AnthropicAuthority:         anthropic,
		model.CopilotIndividualAuthority: copilot,
		model.CopilotBusinessAuthority:   copilot,
		model.CopilotEnterpriseAuthority: copilot,
	}
}

// headerList renders headers as sorted Name=value pairs.
func headerList(header http.Header) string {
	pairs := make([]string, 0, len(header))
	for name, values := range header {
		pairs = append(pairs, http.CanonicalHeaderKey(name)+"="+strconv.Quote(strings.Join(values, ",")))
	}
	slices.Sort(pairs)
	if len(pairs) == 0 {
		return "none"
	}
	return strings.Join(pairs, " ")
}

// TestModelHeadersAreRecorded records, for every model authority of the
// provider table, which of a harness's request headers reach the provider
// and which of the provider's response headers reach the harness, and the
// audit service each call is recorded under.
func TestModelHeadersAreRecorded(t *testing.T) {
	calls := headerCalls()
	var lines []string
	for _, provider := range model.Providers() {
		for _, route := range provider.Models {
			recorded, ok := calls[route.Authority]
			if !ok {
				t.Fatalf("model authority %s has no recorded header calls", route.Authority)
			}
			for _, call := range recorded {
				lines = append(lines, recordHeaders(t, provider.Name, route, call))
			}
		}
	}
	compareRecorded(t, filepath.Join("testdata", "header-outcomes.txt"), lines)
}

func recordHeaders(t *testing.T, provider string, route model.ModelRoute, call headerCall) string {
	t.Helper()
	var received http.Header
	events := &recordedEvents{}
	var auth modelauth.Authenticator = modelauth.Workload{}
	if call.apiKey {
		auth = modelauth.APIKey{Key: recordOpenAIKey}
		if provider == model.ProviderAnthropic {
			auth = modelauth.AnthropicAPIKey{Key: testAnthropicSecret}
		}
	}
	config := model.HandlerConfig{AuditLogger: events, UpstreamURL: testModelUpstream, ProviderAuth: auth,
		Logger: slog.New(slog.DiscardHandler),
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			received = r.Header.Clone()
			response := modelResponse(r, call.answer)
			response.StatusCode, response.Status = call.status, http.StatusText(call.status)
			for name, value := range providerHeaders {
				response.Header.Set(name, value)
			}
			return response, nil
		})}}
	routes := gateway.Dispatcher{route.Authority: {Capability: api.CapabilityModelInference, Tier: audit.TierSemantic,
		Handler: route.New(config), Budgets: budget.NewTracker(), AuditLogger: events,
		ProviderCredential: !call.apiKey}}
	req := httptest.NewRequest(call.method, "https://"+strings.TrimSuffix(route.Authority, ":443")+call.path,
		strings.NewReader(call.body))
	for name, value := range clientHeaders {
		req.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	routes.ServeHTTP(response, req.WithContext(gateway.WithIdentity(req.Context(), testExecutionGrant())))
	recorded := events.taken()
	services := make([]string, 0, len(recorded))
	for _, e := range recorded {
		services = append(services, e.Service)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	return strings.Join([]string{provider + " " + strings.TrimSuffix(route.Authority, ":443") + "/" + call.name,
		strconv.Itoa(response.Code), "provider received: " + headerList(received),
		"harness received: " + headerList(response.Header()), "service=" + strings.Join(services, ",")}, " | ")
}
