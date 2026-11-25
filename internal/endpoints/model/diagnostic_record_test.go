package model_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	model "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

// recordOpenAIKey is the gateway-held OpenAI key of the recorded calls.
const recordOpenAIKey = "sk-fixture-openai-key-0123"

// TestModelDiagnosticsAreRecorded records the validation error an
// administrator's diagnostics observer receives for streamed calls whose
// reply, charge and audit event are the same for both protocols, so the
// validation error is the only output that tells them apart.
func TestModelDiagnosticsAreRecorded(t *testing.T) {
	anthropicStream := strings.Join(fixtureEvents(t, "anthropic-thinking-tool-use.sse"), "")
	responsesStream := strings.Join(fixtureEvents(t, "responses-reasoning.sse"), "")
	negativeUsage := strings.Replace(responsesStream, `"total_tokens":1250`, `"total_tokens":-1`, 1)
	lines := make([]string, 0, 3)
	for _, tc := range []struct {
		name                      string
		anthropic                 bool
		body, contentType, answer string
	}{
		{name: "openai stream with the wrong content type", body: responsesStreamRequest, contentType: jsonMediaType,
			answer: responsesStream},
		{name: "anthropic stream with the wrong content type", anthropic: true, body: anthropicStreamRequest,
			contentType: jsonMediaType, answer: anthropicStream},
		{name: "openai terminal event with negative usage", body: responsesStreamRequest, contentType: eventStream,
			answer: negativeUsage},
	} {
		var validation []string
		config := model.HandlerConfig{AuditLogger: audit.NewLogger(io.Discard), UpstreamURL: testModelUpstream,
			ProviderAuth: modelauth.APIKey{Key: recordOpenAIKey},
			HTTPClient:   respond(http.StatusOK, tc.contentType, tc.answer)(nil),
			Diagnostics: func(d model.ResponseDiagnostic) {
				validation = append(validation, d.ValidationError)
			}}
		if tc.anthropic {
			config.ProviderAuth = modelauth.AnthropicAPIKey{Key: testAnthropicSecret}
		}
		req := httptest.NewRequest(http.MethodPost, target(tc.anthropic, ""), strings.NewReader(tc.body))
		serveModel(tc.anthropic, config, budget.NewTracker(), testExecutionGrant(), httptest.NewRecorder(), req)
		lines = append(lines, tc.name+" | "+strings.Join(validation, " ; "))
	}
	compareRecorded(t, filepath.Join("testdata", "diagnostic-outcomes.txt"), lines)
}
