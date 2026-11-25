package model_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	model "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

// deniedResponseHeaders are the provider response headers that never reach a
// sandbox on an OpenAI-protocol route. The provider sets them below in lower
// case, so the test also pins that matching ignores case.
var deniedResponseHeaders = []string{"Set-Cookie", "Set-Cookie2", "Openai-Organization", "Openai-Project"}

// relayedResponseHeaders are provider headers a sandbox still receives.
var relayedResponseHeaders = []string{"Request-Id", "X-Request-Id", "Retry-After",
	"X-Ratelimit-Remaining-Requests", "Openai-Processing-Ms", "X-Whatever"}

// TestOpenAIRoutesRelayNoIdentifyingProviderHeader serves each shape of
// OpenAI-protocol answer, in each credential mode, and checks that the
// harness gets none of the headers that identify the credential's account or
// set client state, and every other provider header.
func TestOpenAIRoutesRelayNoIdentifyingProviderHeader(t *testing.T) {
	stream := fixture(t, "responses-reasoning.sse")
	modes := map[string]modelauth.Authenticator{
		"workload sign-in": modelauth.Workload{},
		"gateway API key":  modelauth.APIKey{Key: recordOpenAIKey},
		"gateway token":    modelauth.Token{Value: "token-fixture"},
	}
	answers := []struct {
		name, authority, method, path, body, answer, contentType string
		status                                                   int
	}{
		{"JSON", model.OpenAIAuthority, http.MethodPost, chatPath, chatBody, chatSuccess, jsonMediaType, http.StatusOK},
		{"stream", model.OpenAIAuthority, http.MethodPost, testResponsesPath, responsesStreamRequest, stream, eventStream, http.StatusOK},
		{nonSuccessCall, model.OpenAIAuthority, http.MethodPost, chatPath, chatBody, `{"error":{"message":"slow"}}`, jsonMediaType, http.StatusTooManyRequests},
		{"discovery", model.OpenAIAuthority, http.MethodGet, testModelsPath, "", `{"data":[]}`, jsonMediaType, http.StatusOK},
		{"Copilot chat", model.CopilotIndividualAuthority, http.MethodPost, copilotChatPath, chatBody, chatSuccess, jsonMediaType, http.StatusOK},
	}
	for modeName, auth := range modes {
		for _, answer := range answers {
			t.Run(modeName+"/"+answer.name, func(t *testing.T) {
				got := serveWithProviderHeaders(t, auth, answer.authority, answer.method, answer.path,
					answer.body, answer.answer, answer.contentType, answer.status)
				for _, name := range deniedResponseHeaders {
					if values := got.Values(name); len(values) != 0 {
						t.Errorf("the harness received %s=%q", name, values)
					}
				}
				for _, name := range relayedResponseHeaders {
					if got.Get(name) == "" {
						t.Errorf("the harness did not receive %s; got %v", name, got)
					}
				}
			})
		}
	}
}

// serveWithProviderHeaders sends one harness call and returns the response
// headers the harness receives when the provider answers with identifying
// headers, headers a harness may keep and a header the gateway has never
// heard of.
func serveWithProviderHeaders(t *testing.T, auth modelauth.Authenticator, authority, method, path, body,
	answer, contentType string, status int) http.Header {
	t.Helper()
	events := &recordedEvents{}
	config := model.HandlerConfig{AuditLogger: events, UpstreamURL: testModelUpstream, ProviderAuth: auth,
		Logger: slog.New(slog.DiscardHandler),
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			response := modelResponse(r, answer)
			response.StatusCode, response.Status = status, http.StatusText(status)
			// Set the map directly, so these names are not canonical.
			response.Header["content-type"] = []string{contentType}
			response.Header["set-cookie"] = []string{"provider=1"}
			response.Header["set-cookie2"] = []string{"provider=2"}
			response.Header["openai-organization"] = []string{"org-fixture"}
			response.Header["openai-project"] = []string{"proj-fixture"}
			response.Header["request-id"] = []string{"req_provider"}
			response.Header["x-request-id"] = []string{"x-req-provider"}
			response.Header["retry-after"] = []string{"1"}
			response.Header["x-ratelimit-remaining-requests"] = []string{"9"}
			response.Header["openai-processing-ms"] = []string{"12"}
			response.Header["x-whatever"] = []string{"anything"}
			return response, nil
		})}}
	var route model.ModelRoute
	for _, provider := range model.Providers() {
		for _, candidate := range provider.Models {
			if candidate.Authority == authority {
				route = candidate
			}
		}
	}
	if route.New == nil {
		t.Fatalf("no model route for %s", authority)
	}
	routes := gateway.Dispatcher{authority: {Capability: api.CapabilityModelInference, Tier: audit.TierSemantic,
		Handler: route.New(config), Budgets: budget.NewTracker(), AuditLogger: events,
		ProviderCredential: auth.Mode() == modelauth.WorkloadMode}}
	req := httptest.NewRequest(method, "https://"+strings.TrimSuffix(authority, ":443")+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set(contentTypeHeader, jsonMediaType)
	response := httptest.NewRecorder()
	routes.ServeHTTP(response, req.WithContext(gateway.WithIdentity(req.Context(), testExecutionGrant())))
	_, _ = io.Copy(io.Discard, response.Body)
	if response.Code != status {
		t.Fatalf("status %d, want %d: %s", response.Code, status, response.Body.String())
	}
	return response.Header()
}
