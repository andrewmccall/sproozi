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

package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/budget"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/modelauth"
)

const (
	modelsPath     = "/v1/models"
	errorEventType = "error"
)

// allowedPaths lists the upstream API paths this gateway will proxy.
var allowedPaths = []string{"/v1/chat/completions", responsesPath, modelsPath}

const maxModelBodyBytes = 1 << 20
const responsesPath = "/v1/responses"

// HandlerConfig holds all dependencies for Handler.
type HandlerConfig struct {

	// Pricing is administrator-owned and is required whenever a cost cap is set.
	Pricing *PricingTable

	// AuditLogger writes structured audit events for each gateway decision.
	AuditLogger audit.Writer

	// UpstreamURL is the base URL of the upstream model provider.
	UpstreamURL string

	// ProviderAuth is the single administrator-selected upstream authenticator.
	ProviderAuth modelauth.Authenticator

	// HTTPClient is the HTTP client used for upstream requests.
	HTTPClient *http.Client

	// Diagnostics is an administrator-enabled provider-response observer. It
	// receives bounded, redacted errors, never prompts or model output deltas.
	// This does not bypass usage verification or change budget settlement.
	Diagnostics func(ResponseDiagnostic)
}

// Handler is the HTTP handler for the model gateway.
type Handler struct {
	// Config holds all dependencies and configuration for this handler.
	Config   HandlerConfig
	protocol providerProtocol
}

type providerProtocol uint8

const (
	openAIProtocol providerProtocol = iota
	anthropicProtocol
	// AnthropicAuthority is reserved even when its provider is disabled.
	AnthropicAuthority = "api.anthropic.com:443"
)

// NewAnthropicHandler uses the same authorization and accounting as OpenAI,
// with the native Messages protocol selected by trusted gateway composition.
func NewAnthropicHandler(config HandlerConfig) *Handler {
	return &Handler{Config: config, protocol: anthropicProtocol}
}

// requestStreamCheck is used to detect streaming requests before proxying.
type requestStreamCheck struct {
	Stream bool `json:"stream"`
}

// requestTokenBounds captures the bounded output-token fields used by the
// OpenAI chat-completions and responses APIs. A pointer distinguishes an
// omitted field from a field explicitly set to zero, although both result in
// a conservative reservation of all remaining policy capacity.
type requestTokenBounds struct {
	MaxTokens           *int64 `json:"max_tokens"`
	MaxCompletionTokens *int64 `json:"max_completion_tokens"`
	MaxOutputTokens     *int64 `json:"max_output_tokens"`
}

// responseUsage is deliberately strict: a successful inference response must
// include total_tokens so the gateway can settle its admission accurately.
type responseUsage struct {
	Usage *tokenUsage `json:"usage"`
}

type tokenUsage struct {
	TotalTokens        *int64 `json:"total_tokens"`
	PromptTokens       *int64 `json:"prompt_tokens"`
	CompletionTokens   *int64 `json:"completion_tokens"`
	InputTokens        *int64 `json:"input_tokens"`
	OutputTokens       *int64 `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	PromptTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// openAIErrorBody is the OpenAI-compatible error envelope written on denied requests.
type openAIErrorBody struct {
	Error openAIErrorDetail `json:"error"`
}

// openAIErrorDetail carries the error detail fields in an OpenAI error envelope.
type openAIErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// ServeHTTP implements http.Handler. It validates the request, enforces capability
// and budget policies, proxies to the upstream model provider, and writes an audit event.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	e := audit.Event{Timestamp: time.Now().UTC(), Operation: r.URL.Path}

	identity, ok := gateway.IdentityFromContext(ctx)
	if !ok {
		h.deny(w, e, http.StatusUnauthorized, "gateway identity required", "auth_error", "missing_gateway_identity")
		return
	}
	e = identity.AuditEvent(r.URL.Path)
	e.Service, e.SemanticLevel = "openai", "semantic"
	if h.protocol == anthropicProtocol {
		e.Service = "anthropic"
	}
	e.Capability = string(sprooziv1alpha1.CapabilityModelInference)

	policyGen := identity.Policy.Generation
	e.RunID = identity.Run.Namespace + "/" + identity.Run.Name
	e.RunUID = string(identity.Run.UID)
	e.PolicyName = identity.Policy.Name
	e.PolicyGeneration = policyGen
	e.TokenBudget = identity.Policy.Spec.Budgets[sprooziv1alpha1.CapabilityModelInference].MaxUnits

	// 4. Check the request path is allowed.
	if h.protocol == openAIProtocol && !slices.Contains(allowedPaths, r.URL.Path) {
		h.deny(w, e, http.StatusForbidden,
			"path not allowed", "permission_error", "path_not_allowed")
		return
	}

	// 5. Verify model.inference capability is granted by both the run and the policy.
	if !slices.Contains(identity.Run.Spec.Capabilities, sprooziv1alpha1.CapabilityModelInference) ||
		!slices.Contains(identity.Policy.Spec.AllowedCapabilities, sprooziv1alpha1.CapabilityModelInference) {
		h.deny(w, e, http.StatusForbidden,
			"model.inference capability not granted", "permission_error", "capability_not_granted")
		return
	}

	// 6. Responses SSE is buffered until usage is verified. Other streaming
	// protocols are denied before reserving capacity.
	bodyBytes, err := readBounded(r.Body, maxModelBodyBytes)
	if err != nil {
		h.deny(w, e, http.StatusRequestEntityTooLarge,
			"request body exceeds gateway limit", "invalid_request_error", "request_too_large")
		return
	}
	var check requestStreamCheck
	if h.protocol == anthropicProtocol {
		bodyBytes, err = h.prepareAnthropic(r, bodyBytes, identity.Policy.Spec.Budgets[sprooziv1alpha1.CapabilityModelInference])
		if err != nil {
			h.deny(w, e, http.StatusForbidden, err.Error(), "permission_error", "unsupported_request")
			return
		}
	}
	if json.Unmarshal(bodyBytes, &check) == nil && check.Stream && r.URL.Path != responsesPath && h.protocol != anthropicProtocol {
		h.deny(w, e, http.StatusForbidden,
			"streaming responses are not supported by the model gateway",
			"permission_error", "streaming_not_supported")
		return
	}
	providerAuth := h.Config.ProviderAuth
	if providerAuth == nil {
		h.deny(w, e, http.StatusServiceUnavailable, "provider authentication is unavailable", "server_error", "provider_auth_unavailable")
		return
	}
	e.ProviderAuthMode = providerAuth.Mode()
	if providerAuth.Mode() == modelauth.ChatGPTMode {
		if err := validateChatGPTRequest(r, bodyBytes, identity.Policy.Spec.Budgets[sprooziv1alpha1.CapabilityModelInference].MaxCostMicros); err != nil {
			h.deny(w, e, http.StatusForbidden, err.Error(), "permission_error", "chatgpt_request_not_supported")
			return
		}
	}

	// 7. Reserve token capacity atomically before forwarding. Inference
	// requests with an explicit output bound reserve that bound; requests
	// without one reserve all remaining capacity. The models discovery endpoint
	// does not consume model tokens and therefore has no reservation.
	meter := budget.FromContext(ctx)
	var reservation *budget.Reservation
	if r.URL.Path != modelsPath {
		// An explicit output bound is trusted as an admission bound. Without it,
		// reserve all remaining capacity so unknown input plus output cannot
		// oversubscribe the hard ceiling.
		estimate := requestTokenEstimate(bodyBytes)
		if h.protocol == anthropicProtocol {
			// max_tokens bounds output, not input plus cache use. Reserve all
			// remaining capped capacity until trusted total usage is available.
			estimate = 0
		}
		reservation, err = meter.Reserve(estimate, 0)
	}
	if err != nil {
		h.deny(w, e, http.StatusTooManyRequests, err.Error(), "budget_error", "budget_exhausted")
		return
	}

	// 8. Build the upstream request.
	upstreamReq, err := http.NewRequestWithContext(
		ctx, r.Method, h.Config.UpstreamURL+r.URL.RequestURI(), bytes.NewReader(bodyBytes),
	)
	if err != nil {
		if reservation != nil {
			_ = reservation.Release()
		}
		h.deny(w, e, http.StatusInternalServerError,
			"failed to build upstream request", "server_error", "upstream_error")
		return
	}
	h.copyProviderHeaders(upstreamReq.Header, r.Header)
	if err = providerAuth.Authorize(upstreamReq); err != nil {
		if reservation != nil {
			_ = reservation.Release()
		}
		h.deny(w, e, http.StatusServiceUnavailable, "provider authentication is unavailable", "server_error", "provider_auth_unavailable")
		return
	}
	// Let the trusted Go transport negotiate and decode compression. Forwarding
	// a client's Accept-Encoding disables its transparent gzip decoding and can
	// expose compressed bytes to the usage verifier instead of Responses SSE.
	upstreamReq.Header.Del("Accept-Encoding")

	// 9. Forward request to upstream.
	providerClient := *h.Config.HTTPClient
	providerClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := providerClient.Do(upstreamReq)
	if err != nil {
		if reservation != nil {
			_ = reservation.Forfeit()
		}
		h.deny(w, e, http.StatusBadGateway,
			"upstream request failed", "server_error", "upstream_error")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	h.writeProviderResponse(w, r, e, upstreamReq, resp, bodyBytes, check.Stream, reservation)
}

func (h *Handler) writeProviderResponse(
	w http.ResponseWriter, r *http.Request, e audit.Event,
	upstreamReq *http.Request, resp *http.Response, bodyBytes []byte,
	stream bool, reservation *budget.Reservation,
) {
	identity, _ := gateway.IdentityFromContext(r.Context())
	limits := identity.Policy.Spec.Budgets[sprooziv1alpha1.CapabilityModelInference]
	meter := budget.FromContext(r.Context())
	// 10. Buffer the response body and parse token usage from 2xx inference
	// responses. Missing usage is fail-closed: the provider may have consumed
	// the entire reserved bound, so charge that bound before denying the call.
	body, err := readBounded(resp.Body, maxModelBodyBytes)
	var diagnostic ResponseDiagnostic
	if h.Config.Diagnostics != nil {
		credential := strings.TrimPrefix(upstreamReq.Header.Get("Authorization"), "Bearer ")
		if h.protocol == anthropicProtocol {
			credential = upstreamReq.Header.Get("X-Api-Key")
		}
		diagnostic = describeResponse(resp, body, credential)
		diagnostic.RunUID = string(identity.Run.UID)
		defer func() { h.Config.Diagnostics(diagnostic) }()
	}
	if err != nil {
		diagnostic.ValidationError = "response read failed or exceeded body limit"
		if reservation != nil {
			_ = reservation.Forfeit()
		}
		h.deny(w, e, http.StatusBadGateway,
			"failed to read upstream response", "server_error", "upstream_error")
		return
	}

	var tokensUsed int64
	var costMicros int64
	successful := resp.StatusCode >= 200 && resp.StatusCode < 300
	if h.protocol == anthropicProtocol && !successful {
		if reservation != nil {
			_ = reservation.Release()
		}
		h.deny(w, e, resp.StatusCode, "provider rejected request", "api_error", "provider_rejected")
		return
	}
	if successful && r.URL.Path != modelsPath {
		usageResp, anthropic, usageErr := h.providerUsage(body, r.URL.Path, stream, resp.Header.Get("Content-Type"))
		if invalidTokenUsage(usageResp, usageErr, limits) {
			diagnostic.ValidationError = "completed response omitted required token usage"
			if usageErr != nil {
				diagnostic.ValidationError = usageErr.Error()
			}
			if reservation != nil {
				_ = reservation.Forfeit()
			}
			e.UpstreamStatus = resp.StatusCode
			h.deny(w, e, http.StatusBadGateway,
				"upstream response did not include valid token usage",
				"server_error", "usage_unavailable")
			return
		}
		tokensUsed = *usageResp.Usage.TotalTokens
		if limits.MaxCostMicros > 0 {
			var costErr error
			costMicros, costErr = h.providerCost(bodyBytes, usageResp, anthropic)
			if costErr != nil {
				if reservation != nil {
					_ = reservation.Forfeit()
				}
				h.deny(w, e, http.StatusBadGateway, "administrator pricing is unavailable", "server_error", "pricing_unavailable")
				return
			}
		}
		if reservation != nil {
			if err := reservation.Settle(tokensUsed, costMicros); err != nil {
				e.UpstreamStatus = resp.StatusCode
				h.deny(w, e, http.StatusBadGateway,
					"failed to settle model budget reservation",
					"server_error", "budget_error")
				return
			}
		}
	} else if reservation != nil {
		// Provider errors do not represent successful model consumption. Release
		// the admission so a retry can use the remaining budget.
		_ = reservation.Release()
	}

	// 11. Write upstream response headers and body to the client.
	for k, vv := range resp.Header {
		if h.protocol == anthropicProtocol && !strings.EqualFold(k, "Content-Type") && !strings.EqualFold(k, "Request-ID") {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	if stream && r.URL.Path == responsesPath && resp.StatusCode >= 200 && resp.StatusCode < 300 &&
		strings.TrimSpace(resp.Header.Get("Content-Type")) == "" {
		// Only reached after the buffered SSE stream and usage were verified.
		w.Header().Set("Content-Type", "text/event-stream")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)

	// 12. Emit success audit event.
	state := meter.State()
	e.Allowed = true
	e.UpstreamStatus = resp.StatusCode
	e.TokensUsed = tokensUsed
	e.TokensRemaining = max(0, limits.MaxUnits-state.UnitsUsed)
	h.Config.AuditLogger.Log(e)
}

func invalidTokenUsage(
	usageResp responseUsage, usageErr error, limits sprooziv1alpha1.EndpointBudget,
) bool {
	return usageErr != nil ||
		usageResp.Usage == nil ||
		usageResp.Usage.TotalTokens == nil ||
		*usageResp.Usage.TotalTokens < 0 ||
		(limits.MaxCostMicros > 0 && (usageResp.Usage.PromptTokens == nil ||
			usageResp.Usage.CompletionTokens == nil))
}

func requestTokenEstimate(body []byte) int64 {
	var request requestTokenBounds
	if err := json.Unmarshal(body, &request); err != nil {
		return 0
	}
	var estimate int64
	for _, bound := range []*int64{
		request.MaxTokens, request.MaxCompletionTokens, request.MaxOutputTokens,
	} {
		if bound != nil && *bound > estimate {
			estimate = *bound
		}
	}
	return estimate
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("body exceeds %d bytes", limit)
	}
	return data, nil
}

// deny writes an OpenAI-compatible JSON error response, sets the HTTP status, and logs
// a denied audit event.
func (h *Handler) deny(w http.ResponseWriter, e audit.Event, status int, message, errType, code string) {
	e.Allowed = false
	e.DenyReason = message
	h.Config.AuditLogger.Log(e)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if h.protocol == anthropicProtocol {
		_ = json.NewEncoder(w).Encode(map[string]any{"type": errorEventType, "error": map[string]string{"type": errType, "message": message}})
		return
	}
	_ = json.NewEncoder(w).Encode(openAIErrorBody{
		Error: openAIErrorDetail{Message: message, Type: errType, Code: code},
	})
}

func (h *Handler) prepareAnthropic(r *http.Request, body []byte, limits sprooziv1alpha1.EndpointBudget) ([]byte, error) {
	if err := validateAnthropicRequest(r, body, limits.MaxCostMicros > 0); err != nil {
		return nil, err
	}
	if limits.MaxCostMicros == 0 {
		return body, nil
	}
	var request map[string]json.RawMessage
	_ = json.Unmarshal(body, &request)
	var model string
	_ = json.Unmarshal(request["model"], &model)
	if h.Config.Pricing == nil {
		return nil, fmt.Errorf("administrator pricing is unavailable")
	}
	if _, priced := h.Config.Pricing.Models[model]; !priced {
		return nil, fmt.Errorf("administrator pricing is unavailable")
	}
	// Omitted service_tier defaults to auto upstream. Capped admission uses
	// only the administrator's standard pricing, never Priority Tier capacity.
	request["service_tier"] = json.RawMessage(`"standard_only"`)
	return json.Marshal(request)
}

func (h *Handler) copyProviderHeaders(target, source http.Header) {
	for k, vv := range source {
		if slices.Contains([]string{"authorization", "x-api-key", "proxy-authorization", "cookie"}, strings.ToLower(k)) {
			continue
		}
		if h.protocol == anthropicProtocol && !slices.Contains([]string{"content-type", "accept", "anthropic-version", "anthropic-beta"}, strings.ToLower(k)) {
			continue
		}
		for _, v := range vv {
			target.Add(k, v)
		}
	}
	if h.protocol == anthropicProtocol {
		target.Set("Anthropic-Version", "2023-06-01")
	}
}

func (h *Handler) providerUsage(body []byte, path string, stream bool, contentType string) (responseUsage, anthropicUsage, error) {
	if h.protocol == openAIProtocol {
		usage, err := decodeUsage(body, path, stream, contentType)
		return usage, anthropicUsage{}, err
	}
	usage, err := decodeAnthropicUsage(body, stream, contentType)
	input := usage.Total - usage.Output
	normalized := responseUsage{Usage: &tokenUsage{TotalTokens: &usage.Total, PromptTokens: &input, CompletionTokens: &usage.Output}}
	return normalized, usage, err
}

func (h *Handler) providerCost(body []byte, usage responseUsage, anthropic anthropicUsage) (int64, error) {
	if h.Config.Pricing == nil {
		return 0, fmt.Errorf("administrator pricing is unavailable")
	}
	var requestModel struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &requestModel)
	if h.protocol == anthropicProtocol {
		return h.Config.Pricing.costAnthropic(requestModel.Model, anthropic)
	}
	cached := int64(0)
	if usage.Usage.PromptTokensDetails != nil && usage.Usage.PromptTokensDetails.CachedTokens != nil {
		cached = *usage.Usage.PromptTokensDetails.CachedTokens
	}
	return h.Config.Pricing.Cost(requestModel.Model, *usage.Usage.PromptTokens, cached, *usage.Usage.CompletionTokens)
}
