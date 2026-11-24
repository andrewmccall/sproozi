package model_test

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	model "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

// failingChatGPT is a gateway-held ChatGPT sign-in whose Authorize fails.
type failingChatGPT struct{ err error }

func (failingChatGPT) Mode() string                    { return modelauth.ChatGPTMode }
func (a failingChatGPT) Authorize(*http.Request) error { return a.err }

// TestUnavailableProviderAuthentication records what a model request sees
// when the gateway's ChatGPT sign-in cannot authorize it. Guards: handler.go
// forward, where Authorize fails.
func TestUnavailableProviderAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		message string
	}{
		{name: "untyped error", err: errors.New("ChatGPT token exchange failed (HTTP 503); sign in again if access was revoked"),
			message: "provider authentication is unavailable"},
		{name: "store unreadable", err: errors.New("ChatGPT credentials are unavailable"),
			message: "provider authentication is unavailable"},
		{name: "refresh held", err: fmt.Errorf("wrapped: %w", &modelauth.RefreshError{}),
			message: "provider authentication is unavailable: the ChatGPT sign-in could not be renewed (); " +
				"the gateway tries again 10s after the failure"},
		{name: "sign-in refused", err: &modelauth.RefreshError{Refused: true},
			message: "provider authentication is unavailable: the provider refused to renew the ChatGPT sign-in (); " +
				"an administrator must sign in again"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
			defer upstream.Close()
			var log bytes.Buffer
			h := &model.Handler{Config: model.HandlerConfig{ProviderAuth: failingChatGPT{tc.err}, UpstreamURL: upstream.URL,
				HTTPClient: upstream.Client(), AuditLogger: audit.NewLogger(&log)}}
			req := httptest.NewRequest(http.MethodGet, "https://api.openai.com/v1/models", nil)
			req = req.WithContext(gateway.WithIdentity(req.Context(), testExecutionGrant()))
			response := httptest.NewRecorder()
			gateway.Dispatcher{model.OpenAIAuthority: {Capability: api.CapabilityModelInference, Handler: h,
				Budgets: budget.NewTracker()}}.ServeHTTP(response, req)
			want := `{"error":{"message":"` + tc.message + `","type":"server_error","code":"provider_auth_unavailable"}}`
			if response.Code != http.StatusServiceUnavailable || strings.TrimSpace(response.Body.String()) != want {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if calls.Load() != 0 {
				t.Fatal("the request reached the provider")
			}
			if !strings.Contains(log.String(), `"denyReason":"`+tc.message+`"`) {
				t.Fatalf("audit = %s", log.String())
			}
		})
	}
}
